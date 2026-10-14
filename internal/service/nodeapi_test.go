package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

func adminP() *Principal {
	return &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}
}

func ownerP(orgID string) *Principal {
	return &Principal{User: User{ID: "u-owner"}, Access: authz.User{ID: "u-owner", Active: true, Roles: map[string]string{orgID: "owner"}}}
}

func addNode(t *testing.T, n *twoNodes, name, rng string, tags ...string) {
	t.Helper()
	tg := "[]"
	if len(tags) > 0 {
		tg = `["` + strings.Join(tags, `","`) + `"]`
	}
	must(t, n.st.Nodes.Create(context.Background(), store.Node{
		ID: name, Name: name, Slug: name, Role: node.Worker, Tags: tg, RangeCIDR: rng,
		Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
}

func auditCount(t *testing.T, n *twoNodes, action string) int {
	t.Helper()
	es, err := n.orch.SecurityLog(context.Background(), SecurityFilter{Action: action})
	must(t, err)
	return len(es)
}

func TestListAndShowNodes(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	ns, err := n.orch.ListNodes(ctx)
	must(t, err)
	if len(ns) != 2 || ns[0].Role != node.Controller || ns[1].Name != "w1" || ns[0].Envs != 1 || ns[1].Envs != 1 {
		t.Fatalf("nodes = %+v", ns)
	}
	d, err := n.orch.GetNode(ctx, "w1")
	must(t, err)
	if len(d.EnvList) != 1 || d.EnvList[0].Path != "acme/shop/edge" || d.EnvList[0].Org != "acme" {
		t.Errorf("envs = %+v", d.EnvList)
	}
	if r := d.Redacted(); r.Range != "" || r.Facts != nil {
		t.Errorf("redacted = %+v", r)
	}
	if _, err := n.orch.GetNode(ctx, "nope"); err == nil {
		t.Error("an unknown node was found")
	}
	if d2, err := n.orch.GetNode(ctx, "w"); err == nil {
		t.Errorf("a short prefix matched %q", d2.Name) // under 4 characters never matches by prefix
	}
}

// Tags: unknown ones are refused; the ingress tag stays while a scope uses it.
func TestTagNodeByName(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	if _, err := n.orch.TagNode(ctx, adm, "w1", TagSpec{Add: []string{"gpu"}}); err == nil {
		t.Fatal("unknown tag accepted")
	}
	out, err := n.orch.TagNode(ctx, adm, "w1", TagSpec{Add: []string{"ingress", "egress"}})
	must(t, err)
	if !slices.Equal(out.Tags, []string{"egress", "ingress"}) {
		t.Fatalf("tags = %v", out.Tags)
	}
	if _, err := n.orch.TagNode(ctx, ownerP("x"), "w1", TagSpec{Remove: []string{"egress"}}); err == nil {
		t.Error("a non-admin changed tags")
	}
	if c := auditCount(t, n, auditNodeTags); c != 1 {
		t.Errorf("node.tags rows = %d, want 1", c)
	}
}

// An owner pins and moves only within the nodes opened to the org; the
// controller is always open.
func TestOpenPinAndPlacement(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	og, err := n.st.Orgs.GetBySlug(ctx, "acme")
	must(t, err)
	own := ownerP(og.ID)

	if _, err := n.orch.PinNode(ctx, own, "org", og.ID, "w1"); err == nil || !strings.Contains(err.Error(), "not open") {
		t.Fatalf("pin to a closed node = %v", err)
	}
	if _, err := n.orch.NodeForNewEnv(ctx, own, n.stack, "w1"); err == nil {
		t.Fatal("an owner placed an env on a closed node")
	}
	if _, err := n.orch.MoveEnv(ctx, own, n.dev.ID, "w1", false); err == nil {
		t.Fatal("an owner moved an env to a closed node")
	}
	if _, err := n.orch.OpenNode(ctx, adm, "w1", "acme", true); err != nil {
		t.Fatal(err)
	}
	d, _ := n.orch.GetNode(ctx, "w1")
	if !slices.Equal(d.OpenTo, []string{"acme"}) {
		t.Errorf("open_to = %v", d.OpenTo)
	}
	p, err := n.orch.PinNode(ctx, own, "org", og.ID, "w1")
	must(t, err)
	if p.Scope != "acme" || p.Node != "w1" {
		t.Errorf("pin = %+v", p)
	}
	e, err := n.orch.CreateEnv(ctx, n.stack, "qa", EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(t, err)
	if e.NodeID != "w1" {
		t.Errorf("new env on %q, want the pinned w1", e.NodeID)
	}
	if _, err := n.orch.PinNode(ctx, own, "env", n.dev.ID, "w1"); err == nil {
		t.Error("an env was pinned")
	}
	if got, err := n.orch.PinOf(ctx, "stack", n.stack); err != nil || got.Node != "w1" {
		t.Errorf("PinOf stack = %+v, %v", got, err)
	}
	if _, err := n.orch.PinNode(ctx, own, "org", og.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := n.orch.PinOf(ctx, "org", og.ID); got.Node != "" {
		t.Errorf("cleared pin = %+v", got)
	}
	// closing keeps the envs, stops new pins
	if _, err := n.orch.OpenNode(ctx, adm, "w1", "acme", false); err != nil {
		t.Fatal(err)
	}
	if _, err := n.orch.PinNode(ctx, own, "org", og.ID, "w1"); err == nil {
		t.Error("pinned to a node closed again")
	}
	if _, err := n.orch.OpenNode(ctx, adm, "w1", "nope", true); err == nil {
		t.Error("opened to an org that does not exist")
	}
	// an open node serves the owner's move too
	must(t, func() error { _, err := n.orch.OpenNode(ctx, adm, "w1", "acme", true); return err }())
	if _, err := n.orch.MoveEnv(ctx, own, n.dev.ID, "w1", false); err != nil {
		t.Errorf("owner move to an open node = %v", err)
	}
	for a, want := range map[string]int{auditNodeOpen: 3, "placement.pin": 2} {
		if c := auditCount(t, n, a); c != want {
			t.Errorf("%s rows = %d, want %d", a, c, want)
		}
	}
}

func TestIngressAndExits(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	if _, err := n.orch.IngressSet(ctx, adm, "stack", n.stack, "w1"); err == nil || !strings.Contains(err.Error(), "w1 is not tagged ingress") {
		t.Fatalf("untagged ingress = %v", err)
	}
	_, err := n.orch.TagNode(ctx, adm, "w1", TagSpec{Add: []string{"ingress", "egress"}})
	must(t, err)

	got, err := n.orch.IngressGet(ctx, "env", n.dev.ID)
	must(t, err)
	if got.Node != n.ctl.Name || got.From != "default" || got.Scope != "acme/shop/dev" {
		t.Errorf("default ingress = %+v", got)
	}
	if _, err := n.orch.IngressSet(ctx, adm, "stack", n.stack, "w1"); err != nil {
		t.Fatal(err)
	}
	got, _ = n.orch.IngressGet(ctx, "env", n.dev.ID)
	if got.Node != "w1" || got.From != "acme/shop" {
		t.Errorf("inherited ingress = %+v", got)
	}
	if _, err := n.orch.IngressSet(ctx, adm, "env", n.dev.ID, n.ctl.Name); err != nil {
		t.Fatal(err)
	}
	got, _ = n.orch.IngressGet(ctx, "env", n.dev.ID)
	if got.Node != n.ctl.Name || got.From != "acme/shop/dev" {
		t.Errorf("env ingress = %+v", got)
	}
	if _, err := n.orch.IngressSet(ctx, adm, "env", n.dev.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = n.orch.IngressGet(ctx, "env", n.dev.ID); got.Node != "w1" {
		t.Errorf("cleared ingress = %+v", got)
	}
	// a node cannot lose the tag while a scope uses it
	if _, err := n.orch.TagNode(ctx, adm, "w1", TagSpec{Remove: []string{"ingress"}}); err == nil || !strings.Contains(err.Error(), "ingress of acme/shop") {
		t.Errorf("untag in use = %v", err)
	}

	if _, err := n.orch.ExitSet(ctx, adm, "env", n.dev.ID, []string{"w1", "w1"}); err == nil {
		t.Error("a node listed twice")
	}
	ex, err := n.orch.ExitSet(ctx, adm, "env", n.dev.ID, []string{"w1", n.ctl.Name})
	must(t, err)
	if len(ex.Exits) != 2 || ex.Exits[0].Node != "w1" || ex.Exits[0].Position != 1 {
		t.Errorf("exits = %+v", ex)
	}
	d, _ := n.orch.GetNode(ctx, "w1")
	if !slices.Contains(d.ExitFor, "acme/shop/dev") {
		t.Errorf("exit_for = %v", d.ExitFor)
	}
	if ex, _ = n.orch.ExitGet(ctx, "tile", n.devT.ID); len(ex.Exits) != 2 || ex.From != "acme/shop/dev" {
		t.Errorf("tile inherits the env's list: %+v", ex)
	}
	if ex, _ = n.orch.ExitSet(ctx, adm, "env", n.dev.ID, nil); len(ex.Exits) != 0 {
		t.Errorf("cleared exits = %+v", ex)
	}
	addNode(t, n, "plain", "10.231.0.0/16")
	if _, err := n.orch.ExitSet(ctx, adm, "env", n.dev.ID, []string{"plain"}); err == nil || !strings.Contains(err.Error(), "not tagged egress") {
		t.Errorf("untagged exit = %v", err)
	}
	for a, want := range map[string]int{"placement.ingress": 3, "placement.exits": 2} {
		if c := auditCount(t, n, a); c != want {
			t.Errorf("%s rows = %d, want %d", a, c, want)
		}
	}
}

// Remove needs an empty worker and cleans up after it; forget writes a worker
// off and leaves its envs lost, restorable onto a live node.
func TestRemoveForgetRestore(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	if err := n.orch.RemoveNodeRef(ctx, adm, "w1"); err == nil || !strings.Contains(err.Error(), "still runs 1 envs") {
		t.Fatalf("remove with an env = %v", err)
	}
	if err := n.orch.RemoveNodeRef(ctx, adm, n.ctl.Name); err == nil {
		t.Fatal("the controller was removed")
	}
	addNode(t, n, "tmp", "10.230.0.0/16", "egress")
	_, err := n.orch.ExitSet(ctx, adm, "env", n.dev.ID, []string{"tmp"})
	must(t, err)
	must(t, n.orch.RemoveNodeRef(ctx, adm, "tmp"))
	if _, err := n.st.Nodes.Get(ctx, "tmp"); err == nil {
		t.Error("the node row is still there")
	}
	if rungs, _ := n.st.ExitLadders.List(ctx, "env", n.dev.ID); len(rungs) != 0 {
		t.Errorf("exit rungs left: %v", rungs)
	}

	out, err := n.orch.ForgetNodeRef(ctx, adm, "w1")
	must(t, err)
	if len(out.Lost) != 1 || out.Lost[0].Path != "acme/shop/edge" {
		t.Fatalf("forget = %+v", out)
	}
	lost, err := n.orch.LostEnvsOf(ctx, "acme")
	must(t, err)
	if other, _ := n.orch.LostEnvsOf(ctx, "other"); len(lost) != 1 || lost[0].ID != n.edge.ID || len(other) != 0 {
		t.Fatalf("lost = %+v", lost)
	}
	if _, err := n.orch.RestoreEnv(ctx, adm, n.dev.ID, n.ctl.Name); err == nil || !strings.Contains(err.Error(), "not lost") {
		t.Errorf("restoring a live env = %v", err)
	}
	if _, err := n.orch.RestoreEnv(ctx, adm, n.edge.ID, "w1"); err == nil {
		t.Error("restored onto a forgotten node")
	}
	re, err := n.orch.RestoreEnv(ctx, adm, n.edge.ID, n.ctl.Name)
	must(t, err)
	if re.Node != n.ctl.Name || len(re.Tiles) != 1 {
		t.Errorf("restore = %+v", re)
	}
	e, _ := n.st.Environments.Get(ctx, n.edge.ID)
	if e.NodeID != n.ctl.ID || e.Subnet != "" {
		t.Errorf("env on %q subnet %q", e.NodeID, e.Subnet)
	}
	if lost, _ = n.orch.LostEnvsOf(ctx, "acme"); len(lost) != 0 {
		t.Errorf("still lost: %+v", lost)
	}
	for a, want := range map[string]int{auditNodeRemove: 1, auditNodeForget: 1, auditEnvRestore: 1} {
		if c := auditCount(t, n, a); c != want {
			t.Errorf("%s rows = %d, want %d", a, c, want)
		}
	}
}

// A rejoin keeps the node's id, range and envs; the old certs stop at confirm.
func TestRejoinKeepsRangeAndEnvs(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	adm.User.ID = "u-admin"
	if _, err := n.orch.RejoinNode(ctx, adm, n.ctl.Name); err == nil {
		t.Fatal("the controller was rejoined")
	}
	old := store.NodeCert{Serial: "old-1", NodeID: "w1", Role: "worker", NotAfter: time.Now().Add(time.Hour), CreatedAt: time.Now()}
	must(t, n.st.NodeCerts.Create(ctx, old))

	w, _ := n.st.Nodes.Get(ctx, "w1")
	w.WGAddress = "10.207.255.9" // it joined before
	must(t, n.st.Nodes.Update(ctx, w))
	out, err := n.orch.RejoinNode(ctx, adm, "w1")
	must(t, err)
	w, _ = n.st.Nodes.Get(ctx, "w1")
	if w.Status != StateNeedsRejoin || !strings.Contains(out.Command, "--token") {
		t.Fatalf("after rejoin: %s %q", w.Status, out.Command)
	}
	// The box still has the cluster's networks: its tunnel and env bridge.
	req := joinReq(t, out.Token)
	req.Facts = json.RawMessage(`{"interfaces":[{"name":"stackr0","prefixes":["10.207.255.9/24"]},{"name":"br-1","prefixes":["10.208.0.128/24"]}],"stackr_bridges":["br-1"]}`)
	st, err := n.orch.RequestJoin(ctx, httpsMeta, req)
	must(t, err)
	if st.NodeID != "w1" {
		t.Fatalf("the request made node %q, want the existing w1", st.NodeID)
	}
	if w, _ = n.st.Nodes.Get(ctx, "w1"); w.Status != node.StatusPending {
		t.Fatalf("status = %s", w.Status)
	}
	if _, err := n.orch.ConfirmJoin(ctx, adm, "w1"); err != nil {
		t.Fatal(err)
	}
	w, _ = n.st.Nodes.Get(ctx, "w1")
	if w.WGAddress != "10.207.255.9" || w.Status != node.StatusJoined {
		t.Errorf("transit %q status %s", w.WGAddress, w.Status)
	}
	if revoked, _ := n.st.NodeCerts.IsRevoked(ctx, "old-1"); !revoked {
		t.Error("the old certificate still works")
	}
	if es, _ := n.st.Environments.ListByNode(ctx, "w1"); len(es) != 1 {
		t.Errorf("envs on w1 = %d", len(es))
	}
	if c := auditCount(t, n, auditNodeRejoin); c != 1 {
		t.Errorf("node.rejoin rows = %d", c)
	}
	// A new node is checked on its own networks, not on w1's rejoin facts.
	t3, err := n.orch.CreateJoinToken(ctx, adm, JoinTokenSpec{NodeName: "w3"})
	must(t, err)
	st3, err := n.orch.RequestJoin(ctx, httpsMeta, joinReq(t, t3.Token))
	must(t, err)
	if _, err := n.orch.ConfirmJoin(ctx, adm, st3.NodeID); err != nil {
		t.Fatalf("a new node refused over another node's networks: %v", err)
	}
}

// A rejected rejoin (its installer died before confirm) goes back to
// needs-rejoin with its name, address and envs; a fresh rejoin then works.
func TestRejectRejoinKeepsNode(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	w, _ := n.st.Nodes.Get(ctx, "w1")
	w.WGAddress = "10.207.255.9"
	must(t, n.st.Nodes.Update(ctx, w))
	out, err := n.orch.RejoinNode(ctx, adm, "w1")
	must(t, err)
	_, err = n.orch.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token))
	must(t, err)
	must(t, n.orch.RejectJoin(ctx, adm, "w1"))
	w, err = n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	if w.Status != StateNeedsRejoin || w.Name != "w1" || w.WGAddress != "10.207.255.9" {
		t.Fatalf("after reject: %+v", w)
	}
	if es, _ := n.st.Environments.ListByNode(ctx, "w1"); len(es) != 1 {
		t.Errorf("envs on w1 = %d", len(es))
	}
	if s, _ := n.orch.JoinStatus(ctx, out.Token); s.Status != JoinRejected {
		t.Errorf("old token = %s", s.Status)
	}
	out, err = n.orch.RejoinNode(ctx, adm, "w1")
	must(t, err)
	st, err := n.orch.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token))
	must(t, err)
	if st.NodeID != "w1" {
		t.Fatalf("the fresh rejoin made node %q", st.NodeID)
	}
	if _, err := n.orch.ConfirmJoin(ctx, adm, "w1"); err != nil {
		t.Fatal(err)
	}
	if w, _ = n.st.Nodes.Get(ctx, "w1"); w.Status != node.StatusJoined || w.WGAddress != "10.207.255.9" {
		t.Errorf("after fresh rejoin: %s %s", w.Status, w.WGAddress)
	}
}

// A rejoin skips only stackr's own networks; a new node skips none.
func TestReportedPrefixesRejoin(t *testing.T) {
	row := store.JoinToken{Facts: `{"interfaces":[{"name":"stackr0","prefixes":["10.207.255.9/24"]},` +
		`{"name":"br-1","prefixes":["10.208.0.1/24"]},{"name":"br-2","prefixes":["10.208.9.1/24"]},` +
		`{"name":"eth0","prefixes":["192.168.1.5/24"]}],"stackr_bridges":["br-1"]}`}
	str := func(ps []netip.Prefix) string { return fmt.Sprint(ps) }
	if got := str(reportedPrefixes(row, true)); got != "[10.208.9.1/24 192.168.1.5/24]" {
		t.Errorf("rejoin = %s", got)
	}
	if got := reportedPrefixes(row, false); len(got) != 4 {
		t.Errorf("new node = %v", got)
	}
}

func TestClusterStatusFlagsDownNodes(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	// no worker answers in the fixture: the controller is up, w1 is not
	c, err := n.orch.ClusterStatus(ctx)
	must(t, err)
	if len(c.Nodes) != 2 || len(c.Problems) != 1 || !strings.Contains(c.Problems[0], "w1 is unreachable") ||
		len(c.Links) != 1 || c.Links[0].State != "down" {
		t.Fatalf("status = %+v", c)
	}
	if r := c.Redacted(); r.Nodes[1].Range != "" {
		t.Error("the cluster view leaks ranges to members")
	}
}

// An offline worker that answers its liveness port lists as tunnel-down.
func TestListNodesTunnelDown(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	w, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	w.Status = node.StatusOffline
	must(t, n.st.Nodes.Update(ctx, w))
	if n.orch.cluster == nil {
		n.orch.cluster = &clusterPKI{}
	}
	n.orch.cluster.alive = map[string]bool{"w1": true}
	ns, err := n.orch.ListNodes(ctx)
	must(t, err)
	if ns[1].State != NodeTunnelDown {
		t.Errorf("w1 state = %q, want %q", ns[1].State, NodeTunnelDown)
	}
	d, err := n.orch.GetNode(ctx, "w1")
	must(t, err)
	if d.State != NodeTunnelDown {
		t.Errorf("show state = %q", d.State)
	}
}

// A worker's arch and stackr version come from the facts it reported.
func TestListNodesShowsWorkerArch(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.orch.mesh.Report(ctx, "w1", worker.Facts{Arch: "arm64", Version: "0.6.0", Docker: "29.0.0"})
	ns, err := n.orch.ListNodes(ctx)
	must(t, err)
	if ns[1].Arch != "arm64" || ns[1].Docker != "29.0.0" {
		t.Fatalf("w1 = %+v", ns[1])
	}
}
