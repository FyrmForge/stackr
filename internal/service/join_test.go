package service

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/jointoken"
)

var wgKey = base64.StdEncoding.EncodeToString(make([]byte, 32))

// joinWorld is a secWorld with a cluster CA on disk.
func joinWorld(t *testing.T) (*world, *Principal, *clusterca.CA) {
	t.Helper()
	w, adm := secWorld(t)
	ca, err := clusterca.NewCA(time.Now())
	must(t, err)
	must(t, clusterca.SaveCA(w.orch.cfg.DataDir, ca))
	return w, adm, ca
}

func csrPEM(t *testing.T) string {
	t.Helper()
	k, err := clusterca.NewKey()
	must(t, err)
	b, err := clusterca.NewCSR(k)
	must(t, err)
	return string(b)
}

func joinReq(t *testing.T, token string) JoinRequest {
	return JoinRequest{
		Token: token, CSR: csrPEM(t), WGPublicKey: wgKey,
		Facts: json.RawMessage(`{"interfaces":[{"name":"eth0","prefixes":["192.168.50.5/24"]}]}`),
	}
}

var httpsMeta = JoinMeta{IP: "203.0.113.9", UserAgent: "stackrd-test", HTTPS: true}

func mint(t *testing.T, w *world, adm *Principal, name string) JoinTokenOut {
	t.Helper()
	out, err := w.orch.CreateJoinToken(context.Background(), adm, JoinTokenSpec{NodeName: name, Tags: []string{"builder"}})
	must(t, err)
	return out
}

// A new node always carries egress: its default exit can't be picked before
// it exists.
func TestJoinTokenAlwaysEgress(t *testing.T) {
	w, adm, _ := joinWorld(t)
	out, err := w.orch.CreateJoinToken(context.Background(), adm, JoinTokenSpec{NodeName: "w1", Tags: []string{"ingress"}})
	must(t, err)
	if !slices.Equal(out.Tags, []string{"egress", "ingress"}) {
		t.Errorf("tags = %v, want egress added", out.Tags)
	}
	out, err = w.orch.CreateJoinToken(context.Background(), adm, JoinTokenSpec{NodeName: "w2", Tags: []string{"egress"}})
	must(t, err)
	if !slices.Equal(out.Tags, []string{"egress"}) {
		t.Errorf("tags = %v, want egress once", out.Tags)
	}
}

func TestJoinTokenRules(t *testing.T) {
	w, adm, ca := joinWorld(t)
	ctx := context.Background()

	out := mint(t, w, adm, "w1")
	if !strings.HasPrefix(out.Token, jointoken.Prefix) || !strings.Contains(out.Command, out.Token) ||
		!strings.Contains(out.Command, caFingerprint(ca)) || out.Fingerprint != caFingerprint(ca) {
		t.Errorf("out = %+v", out)
	}
	if d := time.Until(out.ExpiresAt); d < 59*time.Minute || d > time.Hour {
		t.Errorf("default ttl = %v", d)
	}
	// only the hash is stored
	if n := count(t, w, `SELECT COUNT(*) FROM join_tokens WHERE hash = ?`, out.Token); n != 0 {
		t.Error("the token is stored in the clear")
	}
	e := one(t, w, jointoken.ActionToken)
	if e.ActorID != adm.User.ID || e.KeyID != "k-1" || strings.Contains(e.Detail, out.Token) {
		t.Errorf("join.token = %+v", e)
	}

	for _, spec := range []JoinTokenSpec{
		{NodeName: "Bad Name"}, {NodeName: "controller"}, {NodeName: "w2", Tags: []string{"gpu"}},
		{NodeName: "w2", TTLMinutes: 24*60 + 1}, {NodeName: "w2", TTLMinutes: -1},
	} {
		if _, err := w.orch.CreateJoinToken(ctx, adm, spec); err == nil {
			t.Errorf("%+v accepted", spec)
		}
	}
	if len(events(t, w, jointoken.ActionToken)) != 1 {
		t.Error("a refused token left a row")
	}

	// single use
	if _, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token)); !isNotFound(err) {
		t.Errorf("second use = %v, want not found", err)
	}
	// unknown
	if _, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, "stkjoin_nope")); !isNotFound(err) {
		t.Errorf("unknown token = %v", err)
	}
	// expired
	exp := mint(t, w, adm, "w2")
	_, err := w.st.DB().Exec(`UPDATE join_tokens SET expires_at = ? WHERE hash = ?`, time.Now().UTC().Add(-time.Minute), jointoken.Hash(exp.Token))
	must(t, err)
	if _, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, exp.Token)); !isNotFound(err) {
		t.Errorf("expired token = %v", err)
	}
	// a name taken by a node cannot be minted again
	if _, err := w.orch.CreateJoinToken(ctx, adm, JoinTokenSpec{NodeName: "controller2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.orch.CreateJoinToken(ctx, adm, JoinTokenSpec{NodeName: "w1"}); err == nil {
		t.Error("minted a token for a pending node's name")
	}
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), errs.ErrNotFound.Error())
}

func TestJoinRequestValidation(t *testing.T) {
	w, adm, _ := joinWorld(t)
	ctx := context.Background()
	out := mint(t, w, adm, "w1")

	if _, err := w.orch.RequestJoin(ctx, JoinMeta{HTTPS: false}, joinReq(t, out.Token)); err == nil {
		t.Error("plain http accepted")
	}
	bad := joinReq(t, out.Token)
	bad.CSR = "nope"
	if _, err := w.orch.RequestJoin(ctx, httpsMeta, bad); err == nil {
		t.Error("bad CSR accepted")
	}
	bad = joinReq(t, out.Token)
	bad.WGPublicKey = "short"
	if _, err := w.orch.RequestJoin(ctx, httpsMeta, bad); err == nil {
		t.Error("bad wireguard key accepted")
	}
	bad = joinReq(t, out.Token)
	bad.Facts = json.RawMessage(`[1]`)
	if _, err := w.orch.RequestJoin(ctx, httpsMeta, bad); err == nil {
		t.Error("non-object facts accepted")
	}
	// none of those burned the token or left a row
	if n := count(t, w, `SELECT COUNT(*) FROM nodes WHERE role = 'worker'`); n != 0 {
		t.Errorf("worker rows = %d", n)
	}
	if _, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token)); err != nil {
		t.Errorf("token burned by bad requests: %v", err)
	}
}

func TestJoinConfirmIssuesCert(t *testing.T) {
	w, adm, ca := joinWorld(t)
	ctx := context.Background()
	t1, t2 := mint(t, w, adm, "w1"), mint(t, w, adm, "w2")

	if _, err := w.orch.JoinStatus(ctx, t1.Token); !isNotFound(err) {
		t.Errorf("status of an unspent token = %v", err)
	}
	st1, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, t1.Token))
	must(t, err)
	r := one(t, w, jointoken.ActionRequest)
	if r.TargetID != st1.NodeID || r.IP != httpsMeta.IP || r.ActorID != "" {
		t.Errorf("join.request = %+v", r)
	}
	if s, _ := w.orch.JoinStatus(ctx, t1.Token); s.Status != JoinPending || s.CertPEM != "" {
		t.Errorf("status = %+v", s)
	}
	pend, err := w.orch.PendingJoins(ctx, adm)
	if err != nil || len(pend) != 1 || pend[0].NodeID != st1.NodeID || pend[0].Name != "w1" || pend[0].Tags[0] != "builder" {
		t.Fatalf("pending = %+v %v", pend, err)
	}

	n1, err := w.orch.ConfirmJoin(ctx, adm, st1.NodeID)
	must(t, err)
	if n1.WGAddress == "" || n1.Status != "joined" {
		t.Errorf("node = %+v", n1)
	}

	s, err := w.orch.JoinStatus(ctx, t1.Token)
	must(t, err)
	blk, _ := pem.Decode([]byte(s.CertPEM))
	if s.Status != JoinConfirmed || blk == nil || s.CAPEM == "" {
		t.Fatalf("status = %+v", s)
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	must(t, err)
	peer, err := clusterca.Verify([]*x509.Certificate{ca.Cert}, []*x509.Certificate{cert}, clusterca.RoleWorker, st1.NodeID, time.Now())
	if err != nil {
		t.Fatalf("cert does not verify: %v", err)
	}
	if n := count(t, w, `SELECT COUNT(*) FROM node_certs WHERE serial = ? AND node_id = ? AND role = 'worker' AND revoked_at IS NULL`, peer.Serial, st1.NodeID); n != 1 {
		t.Errorf("node_certs rows = %d", n)
	}
	c := one(t, w, jointoken.ActionConfirm)
	if c.ActorID != adm.User.ID || c.TargetID != st1.NodeID {
		t.Errorf("node.confirm = %+v", c)
	}
	// boot also issues the controller cert; the node's is the one targeting it
	var ci SecurityEvent
	es, err := w.orch.SecurityLog(context.Background(), SecurityFilter{Action: jointoken.ActionCert, TargetKind: "node"})
	if err != nil || len(es) != 1 {
		t.Fatalf("node cert.issue rows = %d (%v), want 1", len(es), err)
	}
	ci = es[0]
	if ci.TargetID != st1.NodeID || !strings.Contains(ci.Detail, peer.Serial) {
		t.Errorf("cert.issue = %+v", ci)
	}

	// done: nothing to confirm or reject again, and it is off the pending list
	if _, err := w.orch.ConfirmJoin(ctx, adm, st1.NodeID); !isNotFound(err) {
		t.Errorf("second confirm = %v", err)
	}
	if err := w.orch.RejectJoin(ctx, adm, st1.NodeID); !isNotFound(err) {
		t.Errorf("reject after confirm = %v", err)
	}
	if p, _ := w.orch.PendingJoins(ctx, adm); len(p) != 0 {
		t.Errorf("pending = %+v", p)
	}

	// the next node gets another range
	st2, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, t2.Token))
	must(t, err)
	n2, err := w.orch.ConfirmJoin(ctx, adm, st2.NodeID)
	must(t, err)
	if n2.WGAddress == n1.WGAddress {
		t.Errorf("w2 = %+v shares with w1 = %+v", n2, n1)
	}
}

func TestJoinReject(t *testing.T) {
	w, adm, _ := joinWorld(t)
	ctx := context.Background()
	tok := mint(t, w, adm, "w1")
	st, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, tok.Token))
	must(t, err)
	must(t, w.orch.RejectJoin(ctx, adm, st.NodeID))

	e := one(t, w, jointoken.ActionReject)
	if e.ActorID != adm.User.ID || e.TargetID != st.NodeID {
		t.Errorf("node.reject = %+v", e)
	}
	if n := count(t, w, `SELECT COUNT(*) FROM nodes WHERE id = ?`, st.NodeID); n != 0 {
		t.Error("rejected node row remains")
	}
	if s, _ := w.orch.JoinStatus(ctx, tok.Token); s.Status != JoinRejected || s.CertPEM != "" {
		t.Errorf("status = %+v", s)
	}
	if _, err := w.orch.ConfirmJoin(ctx, adm, st.NodeID); !isNotFound(err) {
		t.Errorf("confirm after reject = %v", err)
	}
	if n := count(t, w, `SELECT COUNT(*) FROM node_certs WHERE node_id = ?`, st.NodeID); n != 0 {
		t.Error("a cert was issued to a rejected node")
	}
}

// A failed audit row undoes the act: no cert, still pending.
func TestJoinConfirmRollsBackWithoutAudit(t *testing.T) {
	w, adm, _ := joinWorld(t)
	ctx := context.Background()
	tok := mint(t, w, adm, "w1")
	st, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, tok.Token))
	must(t, err)
	breakAudit(t, w)
	if _, err := w.orch.ConfirmJoin(ctx, adm, st.NodeID); err == nil {
		t.Fatal("confirm succeeded without its audit row")
	}
	if n := count(t, w, `SELECT COUNT(*) FROM node_certs WHERE node_id = ?`, st.NodeID); n != 0 {
		t.Error("cert survived the rollback")
	}
	if n := count(t, w, `SELECT COUNT(*) FROM nodes WHERE id = ? AND status = 'pending'`, st.NodeID); n != 1 {
		t.Error("node left pending state")
	}
}

func TestJoinNeedsCA(t *testing.T) {
	w, adm := secWorld(t)
	// boot makes the CA; take it away to see minting refuse
	for _, f := range []string{"cluster-ca.key", "cluster-ca.crt"} {
		if err := os.Remove(filepath.Join(w.orch.cfg.DataDir, "keys", f)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := w.orch.CreateJoinToken(context.Background(), adm, JoinTokenSpec{NodeName: "w1"})
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("mint without a CA = %v, want conflict", err)
	}
}

// The printed command carries the panel's build, so a stale installer still
// installs it; a plain dev build has no release to name. The confirmed
// status names the node for the installer to keep.
func TestJoinCommandCarriesVersion(t *testing.T) {
	w, adm, _ := joinWorld(t)
	ctx := context.Background()
	for i, c := range []struct{ version, want string }{
		{"v0.6.0", " --version 0.6.0"},
		{"0.6.0-dev.3", " --version 0.6.0-dev.3"},
		{"dev", ""},
		{"", ""},
	} {
		w.orch.cfg.Version = c.version
		out := mint(t, w, adm, "w"+string(rune('a'+i)))
		if got := strings.SplitN(out.Command, "--ca-sha256 "+out.Fingerprint, 2)[1]; got != c.want {
			t.Errorf("version %q: command tail %q, want %q", c.version, got, c.want)
		}
	}

	w.orch.cfg.Version = "dev"
	out := mint(t, w, adm, "named")
	st, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token))
	must(t, err)
	if s, _ := w.orch.JoinStatus(ctx, out.Token); s.NodeName != "" {
		t.Errorf("a pending join names the node: %+v", s)
	}
	_, err = w.orch.ConfirmJoin(ctx, adm, st.NodeID)
	must(t, err)
	if s, err := w.orch.JoinStatus(ctx, out.Token); err != nil || s.NodeName != "named" {
		t.Errorf("confirmed status node name = %q, %v", s.NodeName, err)
	}
}
