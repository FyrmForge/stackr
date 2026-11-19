package panel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
)

// A new panel that fails its trial leaves its exit code and log tail beside
// the database, before the container is removed; TakeFailure reads it once.
// The container's env never reaches the file.
func TestSwapFailKeepsReasonAndLog(t *testing.T) {
	db := filepath.Join(t.TempDir(), "stackr.db")
	if err := os.WriteFile(db, []byte("old db"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := dockerfake.New()
	fake.RunID = "new"
	fake.LogsOut = "boom: migration 19 failed"
	fake.Containers = []docker.Container{{ID: "old", State: "running", Labels: map[string]string{LabelRole: "panel"}}}
	fake.Details = map[string]docker.Detail{"new": {Running: false, ExitCode: 3, Env: []string{"SECRET=hunter2"}}}
	l := New(fake)
	l.Poll, l.Deadline = time.Millisecond, 20*time.Millisecond
	if err := l.Swap(context.Background(), docker.ContainerSpec{Name: "stackr-0.2.0", Image: "img:0.2.0", Env: []string{"DATABASE_PATH=" + db}}); err == nil {
		t.Fatal("swap passed")
	}
	if b, _ := os.ReadFile(FailureFile(db)); strings.Contains(string(b), "hunter2") {
		t.Error("the env is in the failure file")
	}
	f, ok := ReadFailure(db, "0.2.0")
	if !ok || f.ExitCode != 3 || f.Log != "boom: migration 19 failed" || !strings.Contains(f.Reason, "exited") {
		t.Fatalf("failure = %+v, %v", f, ok)
	}
	if _, ok := ReadFailure(db, "0.2.0"); !ok {
		t.Error("reading deleted the file before it was kept")
	}
	if _, ok := ReadFailure(db, "0.3.0"); ok {
		t.Error("another tag's trial read the file")
	}
	RemoveFailure(db)
	if _, err := os.Stat(FailureFile(db)); err == nil {
		t.Error("the file is left after it was removed")
	}
}

// A swap starts without an old trial's file, so a trial that fails to leave
// one cannot show the last trial's reason.
func TestSwapRemovesOldFailureFile(t *testing.T) {
	db := filepath.Join(t.TempDir(), "stackr.db")
	if err := os.WriteFile(db, []byte("old db"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(FailureFile(db), []byte(`{"tag":"0.1.0","reason":"stale"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := dockerfake.New()
	fake.RunID = "new"
	fake.Containers = []docker.Container{{ID: "old", State: "running", Labels: map[string]string{LabelRole: "panel"}}}
	fake.Details = map[string]docker.Detail{"new": {Running: true, Health: "healthy"}}
	l := New(fake)
	l.Poll, l.Deadline = time.Millisecond, 20*time.Millisecond
	_ = l.Swap(context.Background(), docker.ContainerSpec{Name: "stackr-0.2.0", Image: "img:0.2.0", Env: []string{"DATABASE_PATH=" + db}})
	if _, ok := ReadFailure(db, "0.1.0"); ok {
		t.Error("the old trial's file survived the swap start")
	}
}
