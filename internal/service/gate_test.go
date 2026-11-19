package service

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
)

var gateAcct = regexp.MustCompile(`"password":"([^"]+)","username":"([^"]+)"`)

// A tile address gated by secret refs unlocks with the secret's value, and
// changing the secret rebuilds the proxy rules with no container restart.
func TestGateSecretResolvesAndRebuilds(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	api := w.tile(t, "api", true)
	scope := ParamScope{Kind: "env", ID: w.env}
	set := func(pw string) {
		_, err := w.orch.SetParams(ctx, scope, []ParamEntry{
			{Collection: "gate", Name: "user", Kind: "param", Value: "alice"},
			{Collection: "gate", Name: "password", Kind: "secret", Value: pw},
		})
		must(t, err)
	}
	set("one")
	_, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "api.example.com", Extras: domain.Extras{
		BasicAuth: &domain.BasicAuth{User: "${{ params.gate.user }}", Password: "${{ params.gate.password }}"},
	}})
	must(t, err)
	check := func(pw string) {
		t.Helper()
		m := gateAcct.FindStringSubmatch(w.lastPush())
		if m == nil || m[2] != "alice" || bcrypt.CompareHashAndPassword([]byte(m[1]), []byte(pw)) != nil {
			t.Fatalf("push does not carry alice/%s: %v", pw, m)
		}
	}
	check("one")
	set("two")
	check("two")
}

// The panel names a gate's user, and why an address is locked.
func TestDomainGateLockedReason(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	api := w.tile(t, "api", true)
	d, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "api.example.com", Extras: domain.Extras{
		BasicAuth: &domain.BasicAuth{User: "alice", Password: "${{ params.gate.password }}"},
	}})
	must(t, err)
	if u, why, err := w.orch.DomainGate(ctx, d.ID); err != nil || u != "alice" || !strings.Contains(why, "gate.password") {
		t.Errorf("unset: %q %q %v", u, why, err)
	}
	_, err = w.orch.SetParams(ctx, ParamScope{Kind: "env", ID: w.env}, []ParamEntry{{Collection: "gate", Name: "password", Kind: "secret", Value: "x"}})
	must(t, err)
	if u, why, err := w.orch.DomainGate(ctx, d.ID); err != nil || u != "alice" || why != "" {
		t.Errorf("set: %q %q %v", u, why, err)
	}
}
