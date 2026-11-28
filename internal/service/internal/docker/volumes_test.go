package docker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
)

func TestIsVolumeInUse(t *testing.T) {
	for err, want := range map[error]bool{
		fmt.Errorf("x: %w", cerrdefs.ErrConflict):         true,
		errors.New("remove v: volume is in use - [abc]"):  true,
		errors.New("Cannot connect to the Docker daemon"): false,
		fmt.Errorf("x: %w", cerrdefs.ErrNotFound):         false,
		nil: false,
	} {
		if IsVolumeInUse(err) != want {
			t.Errorf("IsVolumeInUse(%v) = %v", err, !want)
		}
	}
}

// An empty label value filters on "key=", never on the key alone: a blank id
// must not match every container.
func TestLabelFilterEmptyValue(t *testing.T) {
	f := labelFilter(map[string]string{"stackr.tile": ""})
	if !f.ExactMatch("label", "stackr.tile=") || f.ExactMatch("label", "stackr.tile") {
		t.Errorf("filter = %v", f.Get("label"))
	}
}

func TestDeviceOf(t *testing.T) {
	for name, c := range map[string]struct {
		opts map[string]string
		want string
	}{
		"plain":    {nil, ""},
		"bound":    {BindOpts("/mnt/d/v"), "/mnt/d/v"},
		"bind+opt": {map[string]string{"type": "none", "o": "rbind,bind", "device": "/x"}, "/x"},
		"tmpfs":    {map[string]string{"type": "tmpfs", "o": "size=1m", "device": "tmpfs"}, ""},
		"nfs":      {map[string]string{"type": "nfs", "o": "addr=h", "device": ":/p"}, ""},
	} {
		if got := DeviceOf(c.opts); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// emptyDir removes files and dotfiles, accepts an empty dir, and fails when
// an rm fails.
func TestEmptyDir(t *testing.T) {
	run := func(dir string) error {
		return exec.Command("sh", "-c", emptyDir(dir)+"true").Run()
	}
	dir := t.TempDir()
	if err := run(dir); err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	for _, n := range []string{"a", ".hidden", "..odd"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(dir); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
	if os.Geteuid() == 0 {
		t.Skip("root removes anything")
	}
	if err := os.MkdirAll(filepath.Join(dir, "locked", "f"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "locked"), 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(filepath.Join(dir, "locked"), 0o700) }()
	if err := run(dir); err == nil {
		t.Fatal("a failed rm did not fail the script")
	}
}
