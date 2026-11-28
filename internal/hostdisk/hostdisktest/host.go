// Package hostdisktest is a fake machine for the paneldb ops: a temp dir that
// stands in for the host, a mount table the fake systemctl edits, and chattr
// that locks a directory by taking its write bit. Tests only.
package hostdisktest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/hostdisk/sqliteops"
)

// Host is one fake machine.
type Host struct {
	T       testing.TB
	Root    string
	DataDir string
	DiskID  string
	Env     hostdisk.Env

	mu       sync.Mutex
	mounted  bool // the bind unit's mount is live
	diskUp   bool // the xfs disk is mounted
	Commands []string
	deferred []func() // the file view of a mount or unmount, applied when the run that made it ends
	// FailOn makes a command fail: the first word of the call, joined with its args.
	FailOn func(call string) error
	// Hook runs before each paneldb-visible command (a test crashes here).
	Hook func(call string) error
}

// New makes the host: /var/lib/stackr with db/ and swap/, an xfs disk mounted
// at the disk's point with the database folder (0755, as Place makes it).
func New(t testing.TB, diskID string) *Host {
	h := &Host{T: t, Root: t.TempDir(), DataDir: "/var/lib/stackr", DiskID: diskID, diskUp: true}
	for _, d := range []string{h.DataDir + "/db", h.DataDir + "/swap", "/etc/systemd/system", "/proc/1",
		hostdisk.MountPoint(diskID) + "/" + diskID + "/" + hostdisk.PanelDBFolder} {
		if err := os.MkdirAll(h.Path(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(h.Path(h.DataDir+"/db"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.Env = hostdisk.Env{Root: h.Root, TempMounts: true, Exec: h.exec, Checkpoint: sqliteops.Checkpoint, Integrity: sqliteops.Integrity}
	h.writeTable()
	return h
}

// Path is a host path under the temp root.
func (h *Host) Path(p string) string { return h.Root + p }

// DB is the host path of the database file as seen through db/.
func (h *Host) DB() string { return h.Path(h.DataDir + "/db/stackr.db") }

// Folder is the disk folder.
func (h *Host) Folder() string {
	return h.Path(hostdisk.MountPoint(h.DiskID) + "/" + h.DiskID + "/" + hostdisk.PanelDBFolder)
}

func (h *Host) dbDir() string { return h.Path(h.DataDir + "/db") }

// Mounted: the bind mount is live.
func (h *Host) Mounted() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.mounted }

// writeTable rewrites the mount table from the state.
func (h *Host) writeTable() {
	var b strings.Builder
	b.WriteString("20 1 8:1 / / rw - ext4 /dev/sda1 rw\n")
	if h.diskUp {
		fmt.Fprintf(&b, "30 20 8:16 / %s rw - xfs /dev/sdb rw\n", hostdisk.MountPoint(h.DiskID))
		if h.mounted {
			fmt.Fprintf(&b, "40 20 8:16 /%s/%s %s rw - xfs /dev/sdb rw\n", h.DiskID, hostdisk.PanelDBFolder, h.DataDir+"/db")
		}
	}
	if err := os.WriteFile(h.Path("/proc/1/mountinfo"), []byte(b.String()), 0o644); err != nil {
		h.T.Fatal(err)
	}
}

// Mount does what the bind unit's start does. The mount table (the host's
// /proc/1/mountinfo) shows it at once, but the file view under /host of the run
// that made it does not change: that run still sees the bare underlay. The next
// run sees the folder.
func (h *Host) Mount() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mounted {
		return nil
	}
	if !h.diskUp {
		return fmt.Errorf("the disk is not mounted")
	}
	h.mounted = true
	h.writeTable()
	h.deferred = append(h.deferred, h.showMount)
	return nil
}

// showMount: the underlay is set aside, the folder shows through db/.
func (h *Host) showMount() {
	if err := os.Rename(h.dbDir(), h.dbDir()+".under"); err != nil {
		h.T.Error(err)
		return
	}
	if err := os.Symlink(h.Folder(), h.dbDir()); err != nil {
		h.T.Error(err)
	}
}

// Unmount does what stopping the bind unit does (also a power cut with the disk gone).
func (h *Host) Unmount() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.mounted {
		return nil
	}
	h.mounted = false
	h.writeTable()
	h.deferred = append(h.deferred, h.hideMount)
	return nil
}

func (h *Host) hideMount() {
	if err := os.Remove(h.dbDir()); err != nil {
		h.T.Error(err)
		return
	}
	if err := os.Rename(h.dbDir()+".under", h.dbDir()); err != nil {
		h.T.Error(err)
	}
}

// settle applies the file views that changed during a run.
func (h *Host) settle() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.deferred {
		f()
	}
	h.deferred = nil
}

// DiskGone takes the disk away: its bind mount goes with it, the xfs mount too.
func (h *Host) DiskGone() {
	_ = h.Unmount()
	h.settle()
	h.mu.Lock()
	h.diskUp = false
	h.writeTable()
	h.mu.Unlock()
}

// DiskBack mounts the xfs disk again (what disk-check does); the bind mount
// follows only when its unit is started.
func (h *Host) DiskBack() {
	h.mu.Lock()
	h.diskUp = true
	h.writeTable()
	h.mu.Unlock()
}

// Locked: db/ (the underlay) has no write bit, which is what chattr +i does to the real one.
func (h *Host) Locked() bool {
	dir := h.dbDir()
	if h.Mounted() {
		dir += ".under"
	}
	fi, err := os.Stat(dir)
	return err == nil && fi.Mode().Perm()&0o200 == 0
}

func (h *Host) exec(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	h.mu.Lock()
	h.Commands = append(h.Commands, call)
	h.mu.Unlock()
	if h.Hook != nil {
		if err := h.Hook(call); err != nil {
			return nil, err
		}
	}
	if h.FailOn != nil {
		if err := h.FailOn(call); err != nil {
			return nil, err
		}
	}
	switch name {
	case "chattr":
		dir := args[len(args)-1]
		if args[0] == "+i" {
			return nil, os.Chmod(dir, 0o500)
		}
		return nil, os.Chmod(dir, 0o700)
	case "nsenter":
		// nsenter -t 1 -m -- systemctl <verb> <unit>
		a := args[len(args)-3:]
		if a[0] != "systemctl" {
			return nil, nil
		}
		if strings.HasSuffix(a[2], hostdisk.BindUnitName(h.DataDir)) {
			switch a[1] {
			case "start":
				return nil, h.Mount()
			case "stop":
				return nil, h.Unmount()
			}
		}
	}
	return nil, nil
}

// Run is one request against this host, as the privileged helper would run it.
func (h *Host) Run(ctx context.Context, r hostdisk.Request) hostdisk.Result {
	defer h.settle()
	return hostdisk.Run(ctx, h.Env, r)
}

// WriteFile puts a file under a host path.
func (h *Host) WriteFile(p string, b []byte) {
	if err := os.MkdirAll(filepath.Dir(h.Path(p)), 0o755); err != nil {
		h.T.Fatal(err)
	}
	if err := os.WriteFile(h.Path(p), b, 0o600); err != nil {
		h.T.Fatal(err)
	}
}
