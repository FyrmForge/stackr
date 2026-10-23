package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// A re-run with another version moves the panel the way its own Admin,
// Update does: the running panel archives itself, the proxy and registry
// follow, the new image's upgrade-swap helper swaps the panel and puts the
// old one back if the new one fails its health check, then the new panel
// rolls the workers. The installer drives it from the host because the panel
// being left runs the old code, which may refuse the target (a dev build).

// move is what a re-run does to a panel already on the box.
type move int

const (
	stay move = iota // same version: nothing
	up               // newer (0.8.2-dev.1 is after 0.8.1, before 0.8.2), or a panel image that is no release
	down             // older: migrations may not reverse
)

// decide compares the panel's version (its image tag, "" when it is not a
// release image) with the one this run installs.
func decide(cur, want string) move {
	if cur == want {
		return stay
	}
	c, w := "v"+strings.TrimPrefix(cur, "v"), "v"+strings.TrimPrefix(want, "v")
	if semver.IsValid(c) && semver.IsValid(w) && semver.Compare(w, c) < 0 {
		return down
	}
	return up
}

// panelVersion is the panel container's id and the release its image is
// tagged with ("" for an image that is not a release). id is "" with no panel.
func panelVersion(ctx context.Context) (id, ver string) {
	ids, _ := read(ctx, "docker", "ps", "-aq", "--filter", "label="+installspec.LabelRole+"=panel")
	if ids == "" {
		return "", ""
	}
	id = strings.Fields(ids)[0]
	img, _ := read(ctx, "docker", "inspect", "-f", "{{.Config.Image}}", id)
	ver, _ = strings.CutPrefix(img, installspec.Repo+":")
	if ver == img {
		ver = ""
	}
	return id, ver
}

func downgradeErr(cur, want string) error {
	return fmt.Errorf("the panel runs %s, newer than %s: refusing to downgrade. Migrations may not reverse, so the older panel may not start on this database. "+
		"Restore the pre-upgrade archive instead (stackr-install restore <archive>), or pass --allow-downgrade", cur, want)
}

type step struct {
	name string
	do   func(context.Context) error
}

// runSteps runs steps in order and stops at the first failure.
func runSteps(ctx context.Context, steps []step) error {
	for _, s := range steps {
		if err := s.do(ctx); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return nil
}

// upgradeSteps moves the panel (oldID) to ver. Each step needs the
// one before: nothing changes until the archive is written, and the workers
// roll only onto a panel that answers its health check.
func (r runner) upgradeSteps(in installspec.Input, oldID, ver string, proxyUp bool) []step {
	image := installspec.Image(ver)
	return []step{
		{"pre-upgrade archive", func(ctx context.Context) error {
			if _, err := r.panelCLI(ctx, oldID, in, "admin", "panel-backup", "--dest", "local"); err != nil {
				return fmt.Errorf("%w; nothing was changed (the CLI on this box must be logged in: %s)", err, docsCLILogin)
			}
			return nil
		}},
		{"proxy", func(ctx context.Context) error { return r.ensureProxy(ctx, image, in, proxyUp) }},
		{"registry", func(ctx context.Context) error { return r.ensureRegistry(ctx, in.DataDir) }},
		{"panel swap", func(ctx context.Context) error { return r.swapPanel(ctx, in, ver) }},
		{"health", func(ctx context.Context) error {
			if r.dry {
				return nil
			}
			return waitHealthy(ctx, "http://"+net.JoinHostPort(in.Bind, installspec.PanelPort)+"/api/health")
		}},
		{"workers", func(ctx context.Context) error {
			id, _ := panelVersion(ctx)
			if id == "" && !r.dry {
				return errors.New("the new panel is not running")
			}
			out, err := r.panelCLI(ctx, id, in, "admin", "upgrade-workers")
			if err != nil && strings.Contains(out, "already running") {
				return nil // the new panel queued it itself
			}
			return err
		}},
	}
}

// upgraderName is the swap helper, as the panel's own Update names it.
const upgraderName = "stackr-upgrader"

// swapPanel runs the target image's `stackrd upgrade-swap` in the foreground:
// it stops the old panel, runs the new one, and starts the old one again if
// the new one does not turn healthy. A failed helper stays for its logs.
func (r runner) swapPanel(ctx context.Context, in installspec.Input, ver string) error {
	if running, err := read(ctx, "docker", "inspect", "-f", "{{.State.Running}}", upgraderName); err == nil {
		if running == "true" {
			return errors.New("an upgrade is already running (container " + upgraderName + ")")
		}
		if err := r.docker(ctx, "rm", "-f", upgraderName); err != nil {
			return err
		}
	}
	spec, err := installspec.SwapSpec(ver, in)
	if err != nil {
		return err
	}
	if err := r.docker(ctx, "run", "--name", upgraderName, "--label", installspec.LabelRole+"=upgrader",
		"-v", "/var/run/docker.sock:/var/run/docker.sock", "-e", installspec.SwapSpecEnv+"="+spec,
		installspec.Image(ver), "upgrade-swap"); err != nil {
		return fmt.Errorf("%w; the old panel runs again, see docker logs %s", err, upgraderName)
	}
	// The new panel's boot may have removed it already (service clearUpgrader).
	_ = r.docker(ctx, "rm", "-f", upgraderName)
	return nil
}

// panelCLI runs the stackr CLI inside panel id with this box's login,
// straight at the panel's port so DNS and the proxy are not needed. The
// output is shown and returned.
func (r runner) panelCLI(ctx context.Context, id string, in installspec.Input, args ...string) (string, error) {
	a := append([]string{"exec",
		"-e", "STACKR_CONFIG=" + filepath.Join(in.DataDir, "cli.json"),
		"-e", "STACKR_SERVER=http://" + net.JoinHostPort(in.Bind, installspec.PanelPort),
		id, "/stackr"}, args...)
	if r.dry {
		r.say("  would run: docker", quote(a))
		return "", nil
	}
	r.say("  stackr", strings.Join(args, " "))
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", a...)
	cmd.Stdout = io.MultiWriter(r.out, &buf)
	cmd.Stderr = cmd.Stdout
	err := cmd.Run()
	return buf.String(), err
}
