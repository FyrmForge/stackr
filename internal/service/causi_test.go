package service

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The trust view names the workers on the old root during a rollover and the
// workers break glass left without a live cert; the node status follows.
func TestClusterTrustView(t *testing.T) {
	o, _ := renewRig(t)
	ctx := context.Background()
	adm := &Principal{User: User{ID: "u-admin"}, Access: authz.User{ID: "u-admin", Active: true, Admin: true}}

	if _, err := o.ClusterTrust(ctx, &Principal{}); err == nil {
		t.Fatal("a non-admin read the trust view")
	}
	cur := clusterca.Fingerprint(o.cluster.ca.Cert)
	v, err := o.ClusterTrust(ctx, adm)
	must(t, err)
	if v.Fingerprint != cur || v.RollingOver || len(v.OnOldRoot) != 0 || len(v.NotRejoined) != 0 || !v.ExpiresAt.After(v.CreatedAt) {
		t.Fatalf("fresh view = %+v", v)
	}

	// a rollover with w1 still on the old root
	now := time.Now()
	must(t, o.store.ClusterCA.Save(ctx, store.ClusterCA{CurrentFP: cur, PreviousFP: "old", RolloverStartedAt: &now}))
	must(t, o.store.NodeCerts.Create(ctx, store.NodeCert{Serial: "s-old", NodeID: "w1", Role: clusterca.RoleWorker,
		NotAfter: now.Add(24 * time.Hour), Issuer: "old", CreatedAt: now}))
	v, err = o.ClusterTrust(ctx, adm)
	must(t, err)
	if !v.RollingOver || len(v.OnOldRoot) != 1 || v.OnOldRoot[0].Name != "w1" {
		t.Fatalf("rolling view = %+v", v)
	}
	// once it holds a new-root cert it is off the list
	must(t, o.store.NodeCerts.Create(ctx, store.NodeCert{Serial: "s-new", NodeID: "w1", Role: clusterca.RoleWorker,
		NotAfter: now.Add(24 * time.Hour), Issuer: cur, CreatedAt: now}))
	if v, _ = o.ClusterTrust(ctx, adm); len(v.OnOldRoot) != 0 {
		t.Fatalf("w1 still on old root: %+v", v)
	}

	// break glass: every cert revoked
	_, err = o.store.NodeCerts.RevokeNode(ctx, "w1", now)
	must(t, err)
	must(t, o.store.ClusterCA.Save(ctx, store.ClusterCA{CurrentFP: cur, BreakGlassAt: &now}))
	v, err = o.ClusterTrust(ctx, adm)
	must(t, err)
	if len(v.NotRejoined) != 1 || v.NotRejoined[0].Name != "w1" || v.BreakGlassAt == nil {
		t.Fatalf("break glass view = %+v", v)
	}
	n, err := o.store.Nodes.Get(ctx, "w1")
	must(t, err)
	if got := o.shownStatus(n); got != NodeNotRejoined {
		t.Fatalf("status = %q", got)
	}
	if m := buildClusterMap([]clusterNodeIn{{Node: n, NotRejoined: true}}); m.Nodes[0].Status != NodeNotRejoined {
		t.Fatalf("map status = %q", m.Nodes[0].Status)
	}

	// a rejoin issues a new cert: back to normal
	must(t, o.store.NodeCerts.Create(ctx, store.NodeCert{Serial: "s-rejoin", NodeID: "w1", Role: clusterca.RoleWorker,
		NotAfter: now.Add(24 * time.Hour), Issuer: cur, CreatedAt: now}))
	if v, _ = o.ClusterTrust(ctx, adm); len(v.NotRejoined) != 0 {
		t.Fatalf("still not rejoined: %+v", v)
	}
	if got := o.shownStatus(n); got == NodeNotRejoined {
		t.Fatal("status still not-rejoined")
	}
}
