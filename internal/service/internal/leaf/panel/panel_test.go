package panel

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
)

func calls(f *dockerfake.Fake) string {
	var b []string
	for _, c := range f.Calls() {
		if c.Method != "List" && c.Method != "Inspect" {
			b = append(b, c.Method+" "+strings.Join(c.Args, " "))
		}
	}
	return strings.Join(b, "; ")
}

func TestSwapKeepsOldUntilNewPasses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		health string
		want   string
	}{
		{"healthy", "Stop old; Run stackr-0.2.0 img:0.2.0; StopRemove old"},
		{"unhealthy", "Stop old; Run stackr-0.2.0 img:0.2.0; StopRemove new; Start old"},
	} {
		fake := dockerfake.New()
		fake.RunID = "new"
		fake.Containers = []docker.Container{
			{ID: "old", State: "running", Labels: map[string]string{LabelRole: "panel"}},
		}
		fake.Details = map[string]docker.Detail{"new": {Running: true, Health: tc.health}}
		l := New(fake)
		l.Poll, l.Deadline = time.Millisecond, 20*time.Millisecond
		err := l.Swap(ctx, docker.ContainerSpec{Name: "stackr-0.2.0", Image: "img:0.2.0"})
		if (err != nil) != (tc.health != "healthy") {
			t.Errorf("%s: err = %v", tc.health, err)
		}
		if got := calls(fake); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.health, got, tc.want)
		}
		if fake.Specs[0].Labels[LabelRole] != "panel" {
			t.Error("new panel not labelled")
		}
	}
}

func TestLaunchGuardAndSpecRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := dockerfake.New()
	l := New(fake)
	spec := docker.ContainerSpec{Name: "stackr-0.2.0", Image: "img:0.2.0", Env: []string{"A=1"}}
	if err := l.Launch(ctx, spec); err != nil {
		t.Fatal(err)
	}
	h := fake.Specs[0]
	if h.Image != "img:0.2.0" || !slices.Equal(h.Cmd, []string{"upgrade-swap"}) {
		t.Errorf("helper = %+v", h)
	}
	t.Setenv(SpecEnv, strings.TrimPrefix(h.Env[0], SpecEnv+"="))
	if got, err := SpecFromEnv(); err != nil || got.Name != spec.Name || got.Env[0] != "A=1" {
		t.Errorf("spec back = %+v %v", got, err)
	}
	fake.Containers = []docker.Container{
		{ID: "h", State: "running", Labels: map[string]string{LabelRole: "upgrader"}},
	}
	if err := l.Launch(ctx, spec); err == nil {
		t.Error("second launch while the helper runs")
	}
}

// An image already on the box is not pulled again.
func TestPullSkipsPresent(t *testing.T) {
	fake := dockerfake.New()
	if err := New(fake).Pull(context.Background(), "img:0.2.0", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range fake.Calls() {
		if c.Method == "Pull" {
			t.Error("pulled an image already present")
		}
	}
}

// spy reads the database files at the moments the swap hands over to a container.
type spy struct {
	*dockerfake.Fake
	db      string
	atStart []byte
	atRun   os.FileMode
}

func (s *spy) Start(ctx context.Context, id string) error {
	s.atStart, _ = os.ReadFile(s.db)
	return s.Fake.Start(ctx, id)
}

func (s *spy) Run(ctx context.Context, sp docker.ContainerSpec) (string, error) {
	if fi, err := os.Stat(s.db + ".pre-0.2.0"); err == nil {
		s.atRun = fi.Mode().Perm()
	}
	// The new panel migrates the database and leaves its own wal behind.
	_ = os.WriteFile(s.db, []byte("migrated"), 0o600)
	_ = os.WriteFile(s.db+"-wal", []byte("new wal"), 0o600)
	return s.Fake.Run(ctx, sp)
}

func swapDB(t *testing.T, health string) (*spy, string, error) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "stackr.db")
	for suf, body := range map[string]string{"": "old db", "-wal": "old wal"} {
		if err := os.WriteFile(db+suf, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fake := dockerfake.New()
	fake.RunID = "new"
	fake.Containers = []docker.Container{{ID: "old", State: "running", Labels: map[string]string{LabelRole: "panel"}}}
	fake.Details = map[string]docker.Detail{"new": {Running: true, Health: health}}
	sp := &spy{Fake: fake, db: db}
	l := New(sp)
	l.Poll, l.Deadline = time.Millisecond, 20*time.Millisecond
	err := l.Swap(context.Background(), docker.ContainerSpec{Name: "stackr-0.2.0", Image: "img:0.2.0", Env: []string{"DATABASE_PATH=" + db}})
	return sp, db, err
}

// A failed gate puts the old database back before the old panel starts, and
// the new panel's wal does not follow it.
func TestSwapFailRestoresDatabase(t *testing.T) {
	sp, db, err := swapDB(t, "unhealthy")
	if err == nil {
		t.Fatal("swap passed")
	}
	if string(sp.atStart) != "old db" {
		t.Errorf("db at old start = %q", sp.atStart)
	}
	if b, _ := os.ReadFile(db + "-wal"); string(b) != "old wal" {
		t.Errorf("wal = %q", b)
	}
	if m, _ := filepath.Glob(db + ".pre-*"); len(m) != 0 {
		t.Errorf("copy left after a restore: %v", m)
	}
}

// A pass deletes the copy; the copy was 0600 while it existed.
func TestSwapPassDropsCopy(t *testing.T) {
	sp, db, err := swapDB(t, "healthy")
	if err != nil {
		t.Fatal(err)
	}
	if sp.atRun != 0o600 {
		t.Errorf("copy mode = %v", sp.atRun)
	}
	if m, _ := filepath.Glob(db + ".pre-*"); len(m) != 0 {
		t.Errorf("copy left after a pass: %v", m)
	}
}

func TestLaunchMountsDataDir(t *testing.T) {
	fake := dockerfake.New()
	err := New(fake).Launch(context.Background(), docker.ContainerSpec{Image: "img:0.2.0", Env: []string{"DATABASE_PATH=/srv/stackr/stackr.db"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fake.Specs[0].Volumes, "/srv/stackr:/srv/stackr") {
		t.Errorf("volumes = %v", fake.Specs[0].Volumes)
	}
}
