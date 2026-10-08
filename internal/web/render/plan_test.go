package render_test

import (
	"encoding/json"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// The org plan view does the same: a share keeps the org it shares with, a
// param its key; a removal row (optional) keeps its own kind.
func TestOrgPlanViewKeepsNames(t *testing.T) {
	var p service.OrgConfigPlan
	if err := json.Unmarshal([]byte(`{"changes":[
		{"kind":"domain","new":"qa.example.com"},
		{"kind":"param","field":"s3.ak","new":"x"},
		{"kind":"share","tile":"ops","new":"ops"},
		{"kind":"share","tile":"ops","optional":true,"key":"share:ops"},
		{"kind":"domain-update","tile":"qa.example.com","field":"include_env","old":"a","new":"b"}
	]}`), &p); err != nil {
		t.Fatal(err)
	}
	pv := render.OrgPlanView(p)
	for i, want := range []struct{ kind, tile, field string }{
		{"create", "domain", ""},
		{"create", "param", "s3.ak"},
		{"create", "share ops", ""},
		{"share", "ops", ""},
		{"domain-update", "qa.example.com", "include_env"},
	} {
		got := pv.Changes[i]
		if got.Kind != want.kind || got.Tile != want.tile || got.Field != want.field {
			t.Errorf("row %d = %q %q %q, want %q %q %q", i, got.Kind, got.Tile, got.Field, want.kind, want.tile, want.field)
		}
	}
}

// A created thing keeps its name in the plan view, with the noun it is; a
// change to an existing route is an update, never a create.
func TestServerPlanViewKeepsNames(t *testing.T) {
	var p service.ServerConfigPlan
	if err := json.Unmarshal([]byte(`{"changes":[
		{"kind":"dest","tile":"qa-offsite","new":"qa-sc-bucket","note":"s3"},
		{"kind":"org-create","tile":"qa","new":"QA Org"},
		{"kind":"route","tile":"pve.example.com","new":"10.0.0.2:8006"},
		{"kind":"domain","new":"qa.example.com"},
		{"kind":"param","field":"s3.ak","new":"x"},
		{"kind":"route-update","tile":"pve.example.com","field":"mode","old":"a","new":"b"}
	]}`), &p); err != nil {
		t.Fatal(err)
	}
	pv := render.ServerPlanView(p)
	for i, want := range []struct{ kind, tile, field string }{
		{"create", "dest qa-offsite", ""},
		{"create", "org qa", ""},
		{"create", "route pve.example.com", ""},
		{"create", "domain", ""},
		{"create", "param", "s3.ak"},
		{"route-update", "pve.example.com", "mode"},
	} {
		got := pv.Changes[i]
		if got.Kind != want.kind || got.Tile != want.tile || got.Field != want.field {
			t.Errorf("row %d = %q %q %q, want %q %q %q", i, got.Kind, got.Tile, got.Field, want.kind, want.tile, want.field)
		}
	}
}
