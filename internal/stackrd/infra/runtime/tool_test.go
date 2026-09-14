package runtime

import (
	"context"
	"io"
	"strings"
	"testing"
)

// A tool container that fails has to be reported as a failure. It was not:
// the wait used WaitConditionNotRunning, which a created-and-not-yet-started
// container already satisfies, so every tool container returned status 0,
// an rsync that copied nothing looked like a completed volume move.
func TestToolContainerReportsExitCode(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Skip("no docker")
	}
	out, wait, err := r.toolContainer(context.Background(), toolOpts{
		image: "alpine:3",
		cmd:   []string{"sh", "-c", "echo hi; echo boom >&2; exit 10"},
	})
	if err != nil {
		t.Skip("no docker:", err)
	}
	b, _ := io.ReadAll(out)
	if string(b) != "hi\n" {
		t.Errorf("stdout = %q, want %q", b, "hi\n")
	}
	err = wait()
	if err == nil {
		t.Fatal("exit 10 reported as success")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %v, want it to carry the command's stderr", err)
	}
}
