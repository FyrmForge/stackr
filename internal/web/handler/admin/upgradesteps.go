package admin

import (
	"fmt"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
)

// upgradeSteps is the four steps of the upgrade with the one it is on marked:
// those before it done, those after it waiting.
func upgradeSteps(p service.UpgradeProgress) []ui.UpdateStep {
	tag := vTag(p.Target)
	labels := []string{"Pulling " + tag, "Backing up the panel", "Restarting on " + tag, "Checking it is healthy"}
	steps := make([]ui.UpdateStep, len(labels))
	for i, l := range labels {
		state := "wait"
		switch n := i + 1; {
		case n < p.Step:
			state = "done"
		case n == p.Step:
			state = "run"
		}
		steps[i] = ui.UpdateStep{Label: l, State: state}
	}
	return steps
}

// vTag is a release tag as the tab names it ("v0.9.1"); the payload may hold it bare.
func vTag(t string) string {
	if t == "" {
		return "the new release"
	}
	if !strings.HasPrefix(t, "v") {
		return "v" + t
	}
	return t
}

// elapsed is "1:04".
func elapsed(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
