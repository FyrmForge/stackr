package setup

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// An apply that never ran because its node was offline says so.
func TestWorkLineSkipped(t *testing.T) {
	if got := workLine(service.Job{State: "skipped"}); got != "The apply was skipped; its node was offline." {
		t.Errorf("line = %q", got)
	}
}
