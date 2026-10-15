package service

import (
	"context"
	"crypto/tls"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// nodeCert is a cert and key for id from the panel's cluster CA.
func nodeCert(t *testing.T, o *Orchestrator, id string) tls.Certificate {
	t.Helper()
	ca, err := clusterca.LoadCA(o.cfg.DataDir)
	must(t, err)
	key, err := clusterca.NewKey()
	must(t, err)
	csr, err := clusterca.NewCSR(key)
	must(t, err)
	iss, err := ca.IssueNode(csr, id, time.Now())
	must(t, err)
	kb, err := clusterca.KeyPEM(key)
	must(t, err)
	own, err := tls.X509KeyPair(iss.CertPEM, kb)
	must(t, err)
	return own
}

// A worker's signed call gets its own mesh spec; a bad signature, an old
// clock, a replayed nonce, a cert from another CA and a node the panel does
// not know are all refused.
func TestRecoverNode(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	must(t, o.store.Nodes.Create(ctx, store.Node{
		ID: "w1", Name: "w1", Slug: "w1", Role: node.Worker, Tags: "[]", RangeCIDR: "10.209.0.0/16", WGAddress: "10.207.255.2",
		WGPublicKey: "wg-w1", Status: node.StatusOffline, CreatedAt: time.Now(),
	}))
	own := nodeCert(t, o, "w1")
	call := func(p worker.RecoverPayload, tamper func(*worker.RecoverRequest)) (worker.MeshSpec, error) {
		if p.Purpose == "" {
			p.Purpose = worker.PurposeRecover
		}
		req, err := worker.SignRecover(own, p)
		must(t, err)
		if tamper != nil {
			tamper(&req)
		}
		a, err := o.RecoverNode(ctx, req)
		if err != nil {
			return worker.MeshSpec{}, err
		}
		// The worker would open it with the roots it trusts: the panel's own.
		return a.Open(o.cluster.roots(), nil, p.Nonce, time.Now())
	}
	now := time.Now().Unix()
	facts := worker.Facts{Interfaces: []worker.Iface{{Name: "enp7s0", Prefixes: []string{"10.0.0.2/32"}}}}

	spec, err := call(worker.RecoverPayload{Time: now, Nonce: "n1", Facts: facts}, nil)
	if err != nil {
		t.Fatalf("good call: %v", err)
	}
	if spec.Address.String() != "10.207.255.2/24" {
		t.Fatalf("spec = %+v", spec)
	}
	o.mesh.mu.Lock()
	got := o.mesh.facts["w1"]
	o.mesh.mu.Unlock()
	if len(got.Interfaces) != 1 {
		t.Fatalf("facts not taken: %+v", got)
	}

	refused := func(name string, p worker.RecoverPayload, tamper func(*worker.RecoverRequest)) {
		t.Helper()
		if _, err := call(p, tamper); !errors.Is(err, errs.ErrRefused) {
			t.Errorf("%s: err = %v, want refused", name, err)
		}
	}
	refused("replay", worker.RecoverPayload{Time: now, Nonce: "n1"}, nil)
	if _, err := call(worker.RecoverPayload{Time: now - 600, Nonce: "n2"}, nil); errors.Is(err, errs.ErrRefused) {
		t.Errorf("old clock: err = %v, want a typed Invalid, not a refusal", err)
	} else if _, ok := errs.IsInvalid(err); !ok {
		t.Errorf("old clock: err = %v, want Invalid", err)
	}
	refused("bad signature", worker.RecoverPayload{Time: now, Nonce: "n3"}, func(r *worker.RecoverRequest) {
		r.Payload = []byte(`{"time":1,"nonce":"x"}`)
	})
	refused("no nonce", worker.RecoverPayload{Time: now}, nil)

	other := newClusterPanel(t) // a cert from another cluster's CA
	foreign := nodeCert(t, other, "w1")
	req, err := worker.SignRecover(foreign, worker.RecoverPayload{Time: now, Nonce: "n4", Purpose: worker.PurposeRecover})
	must(t, err)
	if _, err := o.RecoverNode(ctx, req); !errors.Is(err, errs.ErrRefused) {
		t.Errorf("foreign CA: err = %v", err)
	}
	own = nodeCert(t, o, "w9") // ours, but no such node
	refused("unknown node", worker.RecoverPayload{Time: now, Nonce: "n5"}, nil)

	// The security log has the fetch and the refusals of verified certs;
	// a bad signature or a foreign CA never names a node.
	evs, err := o.SecurityLog(ctx, SecurityFilter{})
	must(t, err)
	var details []string
	for _, e := range evs {
		if e.Action == "node.recover" {
			details = append(details, e.TargetID+" "+e.Detail)
		}
	}
	if len(details) != 5 || !slices.Contains(details, "w1 fetched its mesh: w1") ||
		!slices.Contains(details, "w9 refused: not a confirmed worker") {
		t.Fatalf("node.recover rows = %q", details)
	}
}
