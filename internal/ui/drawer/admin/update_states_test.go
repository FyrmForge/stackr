package admin

import (
	"strings"
	"testing"
)

var testSteps = []UpdateStep{
	{"Pulling v0.8.23", "done"}, {"Backing up the panel", "run"}, {"Restarting on v0.8.23", "wait"}, {"Checking it is healthy", "wait"},
}

// A running upgrade shows its steps and elapsed time, no buttons, and the
// reconnecting line the panel restart needs.
func TestUpdateRunningShowsSteps(t *testing.T) {
	out := renderUpdate(t, UpdateView{Version: "v0.8.22", Checked: true, Latest: "v0.8.23", Upgrading: true, Live: "/-/admin/update", Target: "v0.8.23", Steps: testSteps, Elapsed: "0:21"})
	for _, want := range []string{"Upgrading to v0.8.23", "Elapsed 0:21", "Pulling v0.8.23", "Backing up the panel", "Checking it is healthy", "data-reconnecting", "every 3s"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "/update/check") || strings.Contains(out, "Upgrade to ") {
		t.Errorf("a button while upgrading\n%s", out)
	}
	if n := strings.Count(out, `class="panel`); n != 1 {
		t.Errorf("%d panels, want one card", n)
	}
}

// Every step done: the new panel is up and the helper finishes.
func TestUpdateAllStepsDone(t *testing.T) {
	done := []UpdateStep{{"Pulling v1", "done"}, {"Backing up the panel", "done"}, {"Restarting on v1", "done"}, {"Checking it is healthy", "done"}}
	out := renderUpdate(t, UpdateView{Version: "v1", Checked: true, Latest: "v1", Upgrading: true, Steps: done})
	if !strings.Contains(out, "Upgraded. Finishing the swap.") || strings.Contains(out, "Upgrading to") {
		t.Errorf("done state wrong\n%s", out)
	}
}

// A queued upgrade says why it waits.
func TestUpdateQueued(t *testing.T) {
	out := renderUpdate(t, UpdateView{Version: "v1", Checked: true, Latest: "v2", Newer: true, Upgrading: true, Waits: "Upgrade waits for 2 running jobs"})
	if !strings.Contains(out, "Upgrade queued") || !strings.Contains(out, "Upgrade waits for 2 running jobs") {
		t.Errorf("queued state wrong\n%s", out)
	}
}

// A rolled-back upgrade shows the reason, the exit code and the new panel's
// log in a flat details block, and the Upgrade button comes back.
func TestUpdateFailedKeepsReasonAndLog(t *testing.T) {
	v := UpdateView{
		Version: "v0.8.22", Checked: true, Latest: "v0.8.23", Newer: true,
		LastErr: "Rolled back: the new panel did not pass its trial.",
		Failed:  &UpdateFailure{Reason: "the container exited during the health check", ExitCode: 3, Log: "panic: no such column <x>"},
	}
	v.Run.Action, v.Run.Button, v.Run.Title = "/-/admin/update/run?tag=v0.8.23", "Upgrade to v0.8.23", "Upgrade stackr to v0.8.23"
	out := renderUpdate(t, v)
	for _, want := range []string{"Rolled back", "Exit code 3.", "the container exited during the health check", "<details", "panic: no such column &lt;x&gt;", "Upgrade to v0.8.23", "v0.8.23 is still available."} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if n := strings.Count(out, `class="panel`); n != 1 {
		t.Errorf("%d panels: a box in a box", n)
	}
}

// Idle: up to date, update available, never checked, check failed, dev build.
func TestUpdateIdleStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    UpdateView
		want string
	}{
		{"up to date", UpdateView{Version: "v1", Checked: true, Latest: "v1"}, "Up to date"},
		{"available", UpdateView{Version: "v1", Checked: true, Latest: "v2", Newer: true}, "is available"},
		{"never", UpdateView{Version: "v1"}, "Not checked yet."},
		{"check failed", UpdateView{Version: "v1", CheckErr: "github.com did not answer."}, "Could not check for an update: github.com did not answer."},
		{"dev", UpdateView{Version: "dev", Checked: true}, "A dev build cannot upgrade itself."},
	} {
		if out := renderUpdate(t, tc.v); !strings.Contains(out, tc.want) {
			t.Errorf("%s: no %q in\n%s", tc.name, tc.want, out)
		}
	}
	out := renderUpdate(t, UpdateView{Version: "v1", Checked: true, Latest: "v2", Newer: true})
	if !strings.Contains(out, "What's new") || !strings.Contains(out, "releases/tag/v2") {
		t.Errorf("no What's new link\n%s", out)
	}
}

// Worker rows are flat inside one card, with a summary and one button.
func TestUpdateWorkersFlat(t *testing.T) {
	w := &WorkersView{Rows: []WorkerRow{{Name: "a", Current: "v1"}, {Name: "b", Current: "v0", Behind: true}}, Action: "/-/admin/update/workers", Button: "Update workers"}
	out := renderUpdate(t, UpdateView{Version: "v1", Checked: true, Latest: "v1", Workers: w})
	for _, want := range []string{"1 of 2 up to date", "divide-y", "Update workers"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	if n := strings.Count(out, `class="panel`); n != 2 {
		t.Errorf("%d panels, want 2 cards", n)
	}
}
