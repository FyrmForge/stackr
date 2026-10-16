package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
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

func TestNodeShowsDefaultExit(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	seedNode(t, n, "h4", `["egress"]`, "10.210.0.0/16")
	_, err := n.orch.SetNodeExit(ctx, nodeExitAdmin(), "w1", []string{"h4"})
	must(t, err)
	w, err := n.orch.GetNode(ctx, "w1")
	must(t, err)
	if !slices.Equal(w.DefaultExit, []string{"h4"}) {
		t.Errorf("w1 default_exit = %v", w.DefaultExit)
	}
	h, err := n.orch.GetNode(ctx, "h4")
	must(t, err)
	if h.DefaultExit == nil || len(h.DefaultExit) != 0 {
		t.Errorf("egress default_exit = %#v, want empty list", h.DefaultExit)
	}
	if !slices.Equal(h.ExitFor, []string{"node w1"}) {
		t.Errorf("h4 exit_for = %v", h.ExitFor)
	}
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

// An owner pins only to placements open to the org; the default is open to
// all. An env pin that would move the env is refused until confirmed.
func TestOpenPinAndPlacement(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	og, err := n.st.Orgs.GetBySlug(ctx, "acme")
	must(t, err)
	own := ownerP(og.ID)
	n.placeW1(t, false)

	if err := n.orch.SetPin(ctx, own, "org", og.ID, "w1"); err == nil || !strings.Contains(err.Error(), "not open") {
		t.Fatalf("pin to a closed placement = %v", err)
	}
	if _, err := n.orch.PlacementForNewEnv(ctx, own, n.stack, "w1"); err == nil {
		t.Fatal("an owner placed an env on a closed placement")
	}
	if ok, _ := n.orch.nodeOpenToOrg(ctx, "w1", og.ID); ok {
		t.Fatal("a node holding only a closed placement is open to the org")
	}
	must(t, n.orch.SetPlacementOpen(ctx, adm, "w1", false, []string{"acme"}))
	if ok, _ := n.orch.nodeOpenToOrg(ctx, "w1", og.ID); !ok {
		t.Error("an opened placement does not open its node to the org")
	}
	d, _ := n.orch.GetNode(ctx, "w1")
	if !slices.Equal(d.Placements, []string{"w1"}) {
		t.Errorf("placements on w1 = %v", d.Placements)
	}
	// dev runs on the controller: the org pin moves it, so it waits for the
	// confirm; leaving dev out (as a file that pins it does) lands the pin
	var need *MoveNeeded
	if err := n.orch.SetPin(ctx, own, "org", og.ID, "w1"); !errors.As(err, &need) || len(need.Plans) != 1 || need.Plans[0].Placement != "acme/shop/dev" {
		t.Fatalf("org pin over an env on another node = %v", err)
	}
	_, err = n.orch.PinScope(ctx, own, "org", og.ID, "w1", "", "dev")
	must(t, err)
	p, err := n.orch.PinOf(ctx, "org", og.ID)
	must(t, err)
	if p.Scope != "acme" || p.Placement != "w1" {
		t.Errorf("pin = %+v", p)
	}
	e, err := n.orch.CreateEnv(ctx, n.stack, "qa", EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(t, err)
	if e.NodeID != "w1" || e.PlacementID != mustPlacement(t, n, "w1").ID {
		t.Errorf("new env on %q in %q, want the pinned w1", e.NodeID, e.PlacementID)
	}
	if err := n.orch.SetPin(ctx, own, "env", n.dev.ID, "w1"); err == nil {
		t.Error("an env on the controller was pinned to w1 without a move")
	}
	if got, err := n.orch.PinOf(ctx, "stack", n.stack); err != nil || got.Placement != "w1" {
		t.Errorf("PinOf stack = %+v, %v", got, err)
	}
	if got, err := n.orch.PinOf(ctx, "env", e.ID); err != nil || got.Placement != "w1" {
		t.Errorf("PinOf env = %+v, %v", got, err)
	}
	if _, err := n.orch.PinScope(ctx, own, "org", og.ID, "", "", "edge", "qa"); err != nil { // the envs on w1 stay
		t.Fatal(err)
	}
	if got, _ := n.orch.PinOf(ctx, "org", og.ID); got.Placement != "" {
		t.Errorf("cleared pin = %+v", got)
	}
	// closing keeps the envs, stops new pins
	must(t, n.orch.SetPlacementOpen(ctx, adm, "w1", false, nil))
	if err := n.orch.SetPin(ctx, own, "org", og.ID, "w1"); err == nil {
		t.Error("pinned to a placement closed again")
	}
	if got, _ := n.st.Environments.Get(ctx, e.ID); got.NodeID != "w1" {
		t.Errorf("closing moved the env to %q", got.NodeID)
	}
	if err := n.orch.SetPin(ctx, own, "org", og.ID, "nope"); !errors.As(err, new(errs.Invalid)) {
		t.Errorf("pin to a placement that does not exist = %v", err)
	}
	if c := auditCount(t, n, "placement.pin"); c != 2 {
		t.Errorf("placement.pin rows = %d, want 2", c)
	}
}

func TestIngressAndExits(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	n.placeW1(t, true)
	if _, err := n.orch.IngressSet(ctx, adm, "stack", n.stack, "w1"); err == nil || !strings.Contains(err.Error(), "cannot be an ingress") {
		t.Fatalf("untagged ingress = %v", err)
	}
	_, err := n.orch.TagNode(ctx, adm, "w1", TagSpec{Add: []string{"ingress", "egress"}})
	must(t, err)

	// dev runs on the controller, which is tagged ingress: it enters on its own placement
	got, err := n.orch.IngressGet(ctx, "env", n.dev.ID)
	must(t, err)
	if got.Placement != DefaultPlacement || got.From != "default" || got.Scope != "acme/shop/dev" {
		t.Errorf("default ingress = %+v", got)
	}
	if _, err := n.orch.IngressSet(ctx, adm, "stack", n.stack, "w1"); err != nil {
		t.Fatal(err)
	}
	got, _ = n.orch.IngressGet(ctx, "env", n.dev.ID)
	if got.Placement != "w1" || got.From != "acme/shop" {
		t.Errorf("inherited ingress = %+v", got)
	}
	if _, err := n.orch.IngressSet(ctx, adm, "env", n.dev.ID, DefaultPlacement); err != nil {
		t.Fatal(err)
	}
	got, _ = n.orch.IngressGet(ctx, "env", n.dev.ID)
	if got.Placement != DefaultPlacement || got.From != "acme/shop/dev" {
		t.Errorf("env ingress = %+v", got)
	}
	if _, err := n.orch.IngressSet(ctx, adm, "env", n.dev.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = n.orch.IngressGet(ctx, "env", n.dev.ID); got.Placement != "w1" {
		t.Errorf("cleared ingress = %+v", got)
	}
	// a node cannot lose the tag while a scope uses it
	if _, err := n.orch.TagNode(ctx, adm, "w1", TagSpec{Remove: []string{"ingress"}}); err == nil || !strings.Contains(err.Error(), "ingress of acme/shop") {
		t.Errorf("untag in use = %v", err)
	}

	if _, err := n.orch.ExitSet(ctx, adm, "env", n.dev.ID, []string{"w1", "w1"}); err == nil {
		t.Error("a placement listed twice")
	}
	ex, err := n.orch.ExitSet(ctx, adm, "env", n.dev.ID, []string{"w1", DefaultPlacement})
	must(t, err)
	if len(ex.Exits) != 2 || ex.Exits[0].Placement != "w1" || ex.Exits[0].Position != 1 {
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
	addPlacement(t, n, "plain", "plain", true)
	if _, err := n.orch.ExitSet(ctx, adm, "env", n.dev.ID, []string{"plain"}); err == nil || !strings.Contains(err.Error(), "cannot be an exit") {
		t.Errorf("untagged exit = %v", err)
	}
	for a, want := range map[string]int{"placement.ingress": 3, "placement.exits": 2} {
		if c := auditCount(t, n, a); c != want {
			t.Errorf("%s rows = %d, want %d", a, c, want)
		}
	}
}

// Remove needs an empty worker with no placement on it and cleans up after
// it; forget writes a worker off, prunes its placements' rungs and leaves its
// envs lost, to come back through their placement.
func TestRemoveForgetLost(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	adm := adminP()
	n.placeW1(t, true)
	if _, err := n.orch.RemoveNodeRef(ctx, adm, "w1"); err == nil || !strings.Contains(err.Error(), "still runs 1 envs") {
		t.Fatalf("remove with an env = %v", err)
	}
	if _, err := n.orch.RemoveNodeRef(ctx, adm, n.ctl.Name); err == nil {
		t.Fatal("the controller was removed")
	}
	addNode(t, n, "tmp", "10.230.0.0/16", "egress")
	tmp := addPlacement(t, n, "tmp", "tmp", true)
	_, err := n.orch.ExitSet(ctx, adm, "env", n.dev.ID, []string{"tmp"})
	must(t, err)
	if _, err := n.orch.RemoveNodeRef(ctx, adm, "tmp"); err == nil || !strings.Contains(err.Error(), "Placement tmp") {
		t.Fatalf("remove under a placement = %v", err)
	}
	forgetNow(t, n, "tmp")
	if rungs, _ := n.st.ExitLadders.List(ctx, "env", n.dev.ID); len(rungs) != 0 {
		t.Errorf("exit rungs left: %v", rungs)
	}
	if p, err := n.st.Placements.Get(ctx, tmp.ID); err != nil || p.NodeID != "tmp" {
		t.Errorf("the placement of a forgotten node = %+v, %v", p, err)
	}
	if got, _ := n.orch.PlacementByName(ctx, "tmp"); !got.NodeForgotten || got.Online || got.NodeName != "tmp" {
		t.Errorf("placement on a forgotten node = %+v", got)
	}
	addNode(t, n, "tmp2", "10.229.0.0/16")
	removeNow(t, n, "tmp2")
	if _, err := n.st.Nodes.Get(ctx, "tmp2"); err == nil {
		t.Error("the node row is still there")
	}

	out := forgetNow(t, n, "w1")
	if len(out.Lost) != 1 || out.Lost[0].Path != "acme/shop/edge" || out.Lost[0].Placement != "w1" {
		t.Fatalf("forget = %+v", out)
	}
	lost, err := n.orch.LostEnvsOf(ctx, "acme")
	must(t, err)
	if other, _ := n.orch.LostEnvsOf(ctx, "other"); len(lost) != 1 || lost[0].ID != n.edge.ID || len(other) != 0 {
		t.Fatalf("lost = %+v", lost)
	}
	for a, want := range map[string]int{auditNodeRemove: 1, auditNodeForget: 2} {
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

// A pending machine shows the addresses a person knows it by.
func TestHostAddresses(t *testing.T) {
	got := hostAddresses([]worker.Iface{
		{Name: "lo", Prefixes: []string{"127.0.0.1/8", "::1/128"}},
		{Name: "eth0", Prefixes: []string{"192.168.1.110/24", "fe80::1/64", "2001:db8::5/64"}},
		{Name: "docker0", Prefixes: []string{"172.17.0.1/16"}},
		{Name: "br-1a2b3c4d5e6f", Prefixes: []string{"10.208.0.1/24"}},
		{Name: "stackr0", Prefixes: []string{"10.207.255.1/24"}},
		{Name: "veth12", Prefixes: []string{"169.254.3.1/16"}},
	})
	if fmt.Sprint(got) != "[192.168.1.110/24 2001:db8::5/64]" {
		t.Fatalf("addresses = %v", got)
	}
}

// stackr's own networks are skipped on any join, wg0 on a rejoin only.
func TestReportedPrefixesRejoin(t *testing.T) {
	row := store.JoinToken{Facts: `{"interfaces":[{"name":"stackr0","prefixes":["10.207.255.9/24"]},` +
		`{"name":"br-1","prefixes":["10.208.0.1/24"]},{"name":"br-2","prefixes":["10.208.9.1/24"]},` +
		`{"name":"wg0","prefixes":["10.208.7.1/24"]},` +
		`{"name":"eth0","prefixes":["192.168.1.5/24"]}],"stackr_bridges":["br-1"]}`}
	str := func(is []node.Iface) string {
		var out []string
		for _, i := range is {
			out = append(out, i.Name+"="+i.Prefix.String())
		}
		return fmt.Sprint(out)
	}
	if got := str(reportedPrefixes(row, true)); got != "[br-2=10.208.9.1/24 eth0=192.168.1.5/24]" {
		t.Errorf("rejoin = %s", got)
	}
	if got := str(reportedPrefixes(row, false)); got != "[br-2=10.208.9.1/24 wg0=10.208.7.1/24 eth0=192.168.1.5/24]" {
		t.Errorf("new node = %s", got)
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

// The controller has no worker to ask: its docker version and arch come from
// the daemon it talks to.
func TestListNodesControllerFacts(t *testing.T) {
	n := newTwoNodes(t)
	n.ctlFake.Host = docker.HostInfo{CPUs: 4, MemBytes: 8 << 30, Version: "29.9.0"}
	ns, err := n.orch.ListNodes(context.Background())
	must(t, err)
	if c := ns[0]; c.Role != node.Controller || c.Docker != "29.9.0" || c.Arch == "" || c.CPU != 4 {
		t.Fatalf("controller = %+v", c)
	}
}

// A worker on another build than the panel is behind; one on the panel's
// build is not, and the version it reported shows either way.
func TestListNodesShowsVersionAndBehind(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.orch.cfg.Version = "v9.9.9"
	n.orch.mesh.Report(ctx, "w1", worker.Facts{Arch: "arm64", Version: "v9.8.0"})
	ns, err := n.orch.ListNodes(ctx)
	must(t, err)
	if ns[1].Version != "v9.8.0" || !ns[1].Behind || !ns[1].TooOld {
		t.Fatalf("old worker = %+v", ns[1])
	}
	must(t, n.orch.store.Settings.Set(ctx, prevVersionKey, "v9.8.0"))
	if ns, _ = n.orch.ListNodes(ctx); !ns[1].Behind || ns[1].TooOld || ns[0].TooOld {
		t.Fatalf("previous build = %+v, controller %+v", ns[1], ns[0])
	}
	n.orch.mesh.Report(ctx, "w1", worker.Facts{Arch: "arm64", Version: "v9.9.9"})
	ns, err = n.orch.ListNodes(ctx)
	must(t, err)
	if ns[1].Version != "v9.9.9" || ns[1].Behind || ns[1].TooOld {
		t.Fatalf("current worker = %+v", ns[1])
	}
}
