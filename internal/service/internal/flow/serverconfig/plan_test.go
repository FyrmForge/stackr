package serverconfig_test

import (
	"encoding/json"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
)

func TestPlanSummaryAndJSON(t *testing.T) {
	p := serverconfig.Plan{Plan: planfile.Plan{Changes: []serverconfig.Change{
		{Kind: "org-create", Tile: "acme", Impact: "creates org acme; you become its owner"},
		{Kind: "settings", Field: "acme_email"},
		{Kind: "route-delete", Key: "route:old.io", Optional: true},
	}}}
	if s := p.Summary(); s != "1 to add, 1 to change, 1 removal to review, needs confirm" {
		t.Errorf("summary = %q", s)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var flat planfile.Plan
	if err := json.Unmarshal(b, &flat); err != nil || len(flat.Changes) != 3 || !flat.Risky() {
		t.Errorf("a server plan reads as a planfile.Plan: %+v, %v", flat, err)
	}
	if serverconfig.DefaultPath != "stackr-server.yml" {
		t.Errorf("DefaultPath = %q", serverconfig.DefaultPath)
	}
}
