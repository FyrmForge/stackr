package service

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
)

// joinW1 joins a worker the normal way and returns its node id and cert.
func joinW1(t *testing.T, o *Orchestrator, adm *Principal) (string, *x509.Certificate) {
	t.Helper()
	ctx := context.Background()
	out, err := o.CreateJoinToken(ctx, adm, JoinTokenSpec{NodeName: "w1", Tags: []string{"builder"}})
	must(t, err)
	st, err := o.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token))
	must(t, err)
	_, err = o.ConfirmJoin(ctx, adm, st.NodeID)
	must(t, err)
	return st.NodeID, certOf(t, o, out.Token)
}

func certOf(t *testing.T, o *Orchestrator, token string) *x509.Certificate {
	t.Helper()
	s, err := o.JoinStatus(context.Background(), token)
	must(t, err)
	blk, _ := pem.Decode([]byte(s.CertPEM))
	c, err := x509.ParseCertificate(blk.Bytes)
	must(t, err)
	return c
}

func TestBreakGlass(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	adm := adminP()
	id, oldCert := joinW1(t, o, adm)
	oldCA := o.cluster.ca
	w, _ := o.store.Nodes.Get(ctx, id)
	oldAddr, oldRange := w.WGAddress, w.RangeCIDR
	// A command minted before the break glass: it must die with the root.
	leaked, err := o.RejoinNode(ctx, adm, "w1")
	must(t, err)

	must(t, o.BreakGlass(ctx, adm))

	// New root, no overlap: the old one is gone from disk and from the list.
	cur, err := clusterca.LoadCA(o.cfg.DataDir)
	must(t, err)
	if clusterca.Fingerprint(cur.Cert) == clusterca.Fingerprint(oldCA.Cert) {
		t.Fatal("the root did not change")
	}
	if _, err := clusterca.LoadPrevCA(o.cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a previous root is kept: %v", err)
	}
	if rs := o.cluster.roots(); len(rs) != 1 || clusterca.Fingerprint(rs[0]) != clusterca.Fingerprint(cur.Cert) {
		t.Errorf("trust list = %d roots", len(rs))
	}
	if _, err := clusterca.Verify(o.cluster.roots(), []*x509.Certificate{oldCert}, clusterca.RoleWorker, id, time.Now()); err == nil {
		t.Error("a cert from the old root is still accepted")
	}
	row, ok, err := o.store.ClusterCA.Get(ctx)
	if err != nil || !ok || row.BreakGlassAt == nil || row.CurrentFP != clusterca.Fingerprint(cur.Cert) || row.PreviousFP != "" || row.RollingOver() {
		t.Errorf("cluster_ca = %+v %v %v", row, ok, err)
	}
	// The controller cert and its announced serial come from the new root.
	own, err := x509.ParseCertificate(o.cluster.cert().Certificate[0])
	must(t, err)
	if _, err := clusterca.Verify(o.cluster.roots(), []*x509.Certificate{own}, clusterca.RoleController, "", time.Now()); err != nil {
		t.Errorf("controller cert: %v", err)
	}
	if s := o.cluster.serials(); len(s) != 1 || s[0] != serialOf(own) {
		t.Errorf("announced serials = %v", s)
	}
	// The worker is not rejoined, keeps its row, and its old cert is revoked.
	w, _ = o.store.Nodes.Get(ctx, id)
	if w.Status != StateNeedsRejoin || w.WGAddress != oldAddr || w.RangeCIDR != oldRange {
		t.Errorf("worker after = %s %s %s", w.Status, w.WGAddress, w.RangeCIDR)
	}
	if r, _ := o.store.NodeCerts.IsRevoked(ctx, serialOf(oldCert)); !r {
		t.Error("the worker's old cert is not revoked")
	}
	if n := auditCount2(t, o, securitylog.CABreakGlass); n != 2 {
		t.Errorf("ca.break_glass rows = %d, want start and done", n)
	}
	if st, err := o.BreakGlassState(ctx, adm); err != nil || st.At == nil || len(st.NotRejoined) != 1 || st.NotRejoined[0] != "w1" {
		t.Errorf("state = %+v %v", st, err)
	}
	// A second one right after is refused.
	if err := o.BreakGlass(ctx, adm); err == nil {
		t.Error("a second break glass within the gap was accepted")
	}
	// The command from before is dead.
	if _, err := o.RequestJoin(ctx, httpsMeta, joinReq(t, leaked.Token)); !isNotFound(err) {
		t.Errorf("a pre-break-glass command still works: %v", err)
	}

	// Rejoin: a fresh command per node, carrying the new root.
	out, err := o.RejoinNode(ctx, adm, "w1")
	must(t, err)
	if out.Fingerprint != clusterca.Fingerprint(cur.Cert) || !strings.Contains(out.Command, out.Fingerprint) {
		t.Errorf("rejoin command fingerprint = %q", out.Fingerprint)
	}
	st, err := o.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token))
	must(t, err)
	if st.NodeID != id {
		t.Fatalf("the rejoin made node %q, want %q", st.NodeID, id)
	}
	// Single use: a leak after the node joined opens nothing.
	if _, err := o.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token)); !isNotFound(err) {
		t.Errorf("the command worked twice: %v", err)
	}
	if _, err := o.ConfirmJoin(ctx, adm, id); err != nil {
		t.Fatal(err)
	}
	w, _ = o.store.Nodes.Get(ctx, id)
	if w.Status != "joined" || w.WGAddress != oldAddr || w.RangeCIDR != oldRange || w.Name != "w1" {
		t.Errorf("worker after rejoin = %+v", w)
	}
	nc := certOf(t, o, out.Token)
	p, err := clusterca.Verify(o.cluster.roots(), []*x509.Certificate{nc}, clusterca.RoleWorker, id, time.Now())
	if err != nil || p.Issuer != clusterca.Fingerprint(cur.Cert) {
		t.Errorf("rejoined cert: %v %+v", err, p)
	}
	if n := auditCount2(t, o, securitylog.NodeRejoined); n != 1 {
		t.Errorf("node.rejoined rows = %d", n)
	}
	if st, _ := o.BreakGlassState(ctx, adm); len(st.NotRejoined) != 0 {
		t.Errorf("still not rejoined: %v", st.NotRejoined)
	}
}

func auditCount2(t *testing.T, o *Orchestrator, action string) int {
	t.Helper()
	es, err := o.SecurityLog(context.Background(), SecurityFilter{Action: action})
	must(t, err)
	return len(es)
}
