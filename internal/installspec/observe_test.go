package installspec

import (
	"slices"
	"testing"
)

// Logs add the supervisor's -logs flags and change the spec stamp, so
// flipping the knob recreates the container.
func TestObserveLogsFlags(t *testing.T) {
	off := Observe("v0.8.3", "/d", ObserveOpts{Retention: "30d", MinFree: 5})
	on := Observe("v0.8.3", "/d", ObserveOpts{Retention: "30d", MinFree: 5, Logs: true, LogsRetention: "7d", LogsMax: 10})
	if slices.Contains(off.Cmd, "-logs") {
		t.Errorf("off: %v", off.Cmd)
	}
	for _, f := range []string{"-logs", "-logs-retention=7d", "-logs-max-size=10"} {
		if !slices.Contains(on.Cmd, f) {
			t.Errorf("missing %s in %v", f, on.Cmd)
		}
	}
	if off.Labels[LabelSpec] == on.Labels[LabelSpec] {
		t.Error("spec stamp ignores logs")
	}
}
