package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
)

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
	bind := hostdisk.BindUnitName(dataDir)
	if _, err := os.Stat(systemdDir + "/" + bind); err == nil {
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
	for _, u := range units {
		if err := startUnit(ctx, u); err != nil {
			errs = append(errs, err)
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
