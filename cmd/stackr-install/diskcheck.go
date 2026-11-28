package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/paneldb"
)

// `stackr-install disk-check` brings a node disk back without a reboot. The
// mount units are wanted by local-fs.target only, so a disk attached after
// boot is not picked up, and a panel whose database is on that disk is down,
// so nothing in the panel can be clicked. This starts the xfs mount units and
// the database's bind unit and stops there: Docker's restart loop brings the
// panel up by itself.

// systemdDir and startUnit are variables so a test needs no systemd.
var (
	systemdDir = "/etc/systemd/system"
	startUnit  = func(ctx context.Context, unit string) error {
		out, err := exec.CommandContext(ctx, "systemctl", "start", unit).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl start %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// restartUnit runs after a dead mount was cleared: a unit systemd still
	// counts as active would make a plain start a no-op.
	restartUnit = func(ctx context.Context, unit string) error {
		out, err := exec.CommandContext(ctx, "systemctl", "restart", unit).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl restart %s: %w: %s", unit, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	mountTable = func() []hostdisk.Mount {
		b, _ := os.ReadFile("/proc/self/mountinfo")
		return hostdisk.ParseMounts(b)
	}
	sysBlock = "/sys/dev/block"
)

const heldHint = "A running container still holds the old disk. Stop it, or wait for the panel to stop its tiles, then run this again."

// unitWhere is the mount point a stackr mount unit file names ("" when none).
func unitWhere(unit string) string {
	b, _ := os.ReadFile(filepath.Join(systemdDir, unit))
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "Where="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// sysBlockUp reports whether sysfs is readable here; without it nothing can be
// told gone, so nothing is unmounted.
func sysBlockUp() bool {
	_, err := os.Stat(sysBlock)
	return err == nil
}

// devGone: the device left sysfs (false when sysfs itself is not there).
func devGone(dev string) bool {
	if !sysBlockUp() {
		return false
	}
	_, err := os.Lstat(filepath.Join(sysBlock, dev))
	return errors.Is(err, os.ErrNotExist)
}

// clearDeadMount lazily unmounts every mount of the device behind where when
// that device left sysfs (a pulled disk): the mount point and Docker's volume
// binds, last first. It returns the points it unmounted (nil when none).
func clearDeadMount(ctx context.Context, where string) []string {
	ms := mountTable()
	dev := ""
	for _, m := range ms {
		if m.Point == where {
			dev = m.Dev
		}
	}
	if dev == "" || !devGone(dev) {
		return nil
	}
	var gone []string
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Dev == dev {
			_ = abandonUnmount(ctx, ms[i].Point)
			gone = append(gone, ms[i].Point)
		}
	}
	return gone
}

// pointMounted: where is mounted and its device is still in sysfs, so a dead
// mount that survived is not "started".
func pointMounted(where string) bool {
	for _, m := range mountTable() {
		if m.Point == where {
			return !devGone(m.Dev)
		}
	}
	return false
}

func diskCheck(ctx context.Context, args []string, out io.Writer) error {
	if len(args) > 0 {
		return errors.New("usage: stackr-install disk-check (no arguments; STACKR_DATA_DIR names the data dir)")
	}
	dataDir := os.Getenv("STACKR_DATA_DIR")
	if dataDir == "" {
		dataDir = "/var/lib/stackr"
	}
	return startDiskUnits(ctx, dataDir, out)
}

// diskUnits are the units to start, in order: every xfs disk unit stackr wrote,
// then the database bind unit when it has one.
func diskUnits(dataDir string) ([]string, error) {
	ents, err := os.ReadDir(systemdDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // no systemd here: no units
	}
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(hostdisk.UnitName(hostdisk.XFSRoot+"/x"), "x.mount")
	var units []string
	for _, e := range ents {
		if n := e.Name(); strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".mount") {
			units = append(units, n)
		}
	}
	// The bind unit only while the journal says the database is on a disk (or a
	// move is going): after a give-up or a move back a stale one must not mount.
	j, jerr := paneldb.ReadJournal(dataDir)
	onDisk := jerr == nil && (j.Phase == paneldb.PhaseMoved || j.Moved ||
		((j.Direction == paneldb.DirIn || j.Direction == paneldb.DirBack) && j.Phase != paneldb.PhaseNone))
	bind := hostdisk.BindUnitName(dataDir)
	if _, err := os.Stat(systemdDir + "/" + bind); err == nil && onDisk {
		units = append(units, bind)
	}
	return units, nil
}

// startDiskUnits starts them all, each once, and reports every failure.
func startDiskUnits(ctx context.Context, dataDir string, out io.Writer) error {
	units, err := diskUnits(dataDir)
	if err != nil {
		return err
	}
	if len(units) == 0 {
		_, _ = fmt.Fprintln(out, "no node disk units here: nothing to start")
		return nil
	}
	var errs []error
	cleared := map[string]bool{} // mount points unmounted in this run
	for _, u := range units {
		where := unitWhere(u)
		start := startUnit
		if where != "" {
			for _, p := range clearDeadMount(ctx, where) {
				cleared[p] = true
			}
			if cleared[where] { // a bind unit sits on a point the xfs pass already cleared
				start = restartUnit
			}
		}
		err := start(ctx, u)
		held := false
		if err == nil && where != "" && !pointMounted(where) {
			err, held = fmt.Errorf("%s is not mounted after starting %s", where, u), true
		}
		if err != nil {
			errs = append(errs, err)
			if held || cleared[where] {
				_, _ = fmt.Fprintln(out, heldHint)
			}
			continue
		}
		_, _ = fmt.Fprintln(out, "started", u)
	}
	if len(errs) == 0 {
		_, _ = fmt.Fprintln(out, "Docker's restart loop brings the panel up on its own now. Watch it with: docker ps")
		if j, err := paneldb.ReadJournal(dataDir); err == nil && j.Phase == paneldb.PhaseMoved {
			_, _ = fmt.Fprintln(out, "The panel database is on node disk", j.DiskID+".")
		}
	}
	return errors.Join(errs...)
}
