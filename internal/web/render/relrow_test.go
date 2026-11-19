package render

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

func TestRelPlanRowViewOnly(t *testing.T) {
	r := service.WindowRow{Kind: service.RowPlan, State: service.StateWaiting}
	if got := RelPlanRow(r, "org config", "/x", true).Link.Label; got != "View plan" {
		t.Fatalf("viewer label = %q", got)
	}
	if got := RelPlanRow(r, "org config", "/x", false).Link.Label; got != "Review plan" {
		t.Fatalf("owner label = %q", got)
	}
}
