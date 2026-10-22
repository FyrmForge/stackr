package installspec

import (
	"slices"
	"strconv"
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

// The memory cap limits the container, and each store gets its cache share
// of it: 60% for metrics alone, 36/24 with logs. A changed cap recreates.
func TestObserveMemory(t *testing.T) {
	off := Observe("v0.8.3", "/d", ObserveOpts{Retention: "30d", MemoryMB: 1000})
	on := Observe("v0.8.3", "/d", ObserveOpts{Retention: "30d", MemoryMB: 1000, Logs: true})
	if off.MemoryMB != 1000 || !slices.Contains(off.RunArgs(), "1000m") {
		t.Errorf("limit: %d %v", off.MemoryMB, off.RunArgs())
	}
	mb := int64(1000) << 20
	for c, want := range map[*Container][]string{
		&off: {"-memory=" + strconv.FormatInt(mb*60/100, 10)},
		&on:  {"-memory=" + strconv.FormatInt(mb*36/100, 10), "-logs-memory=" + strconv.FormatInt(mb*24/100, 10)},
	} {
		for _, f := range want {
			if !slices.Contains(c.Cmd, f) {
				t.Errorf("missing %s in %v", f, c.Cmd)
			}
		}
	}
	if Observe("v0.8.3", "/d", ObserveOpts{Retention: "30d", MemoryMB: 2000}).Labels[LabelSpec] == off.Labels[LabelSpec] {
		t.Error("spec stamp ignores memory")
	}
}
