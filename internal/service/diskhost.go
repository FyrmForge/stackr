package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/disk"
)

// The disk host runs one hostdisk command on a node: in a privileged helper
// container (the host's / at /host), or in process on the controller when the
// panel is not an installed container.

const (
	// diskHelperRole is the helper's role label. It is not host-helper: the
	// worker rollout stops every host-helper, and a drawer read must survive it.
	diskHelperRole = "disk-helper"
	// labelDiskDeadline is the unix second after which a helper is a leftover.
	labelDiskDeadline = "stackr.disk-deadline"
	diskHelperPrefix  = "stackr-disk-"

	diskShortWait = 30 * time.Second // verify, folder, drop, a readdir
	diskLongWait  = 5 * time.Minute  // a sizes walk
	diskPullWait  = 10 * time.Minute // pulling the helper image
	diskCleanWait = 15 * time.Second // removing the helper after a cancel
	diskSlack     = time.Minute      // past its deadline, a helper is swept
)

type diskHost struct {
	o           *Orchestrator
	short, long time.Duration
	pullWait    time.Duration
	// image is the helper image on a node; tests replace it.
	image  func(ctx context.Context) (string, error)
	pulled sync.Map // node id + image ref -> true
}

func newDiskHost(o *Orchestrator) *diskHost {
	return &diskHost{o: o, short: diskShortWait, long: diskLongWait, pullWait: diskPullWait, image: o.helperImage}
}

// InProcessDisk runs every command on e, whatever the node. For tests and
// the harness, where there is no helper image.
func InProcessDisk(e hostdisk.Env) disk.Host { return processDisk{e} }

type processDisk struct{ env hostdisk.Env }

func (p processDisk) Disk(ctx context.Context, _ string, r hostdisk.Request) (hostdisk.Result, error) {
	return hostdisk.Run(ctx, p.env, r), nil
}

// WithDiskHost replaces the disk host. nil keeps the helper runner.
func WithDiskHost(h disk.Host) Option {
	return func(o *options) { o.diskHost = h }
}

// installedPanel: the panel runs as an installed container, so the host's
// paths are not its own. A source build or a dev run sees the host directly.
func (c Config) installedPanel() bool {
	return c.PanelSpec != nil && c.FakeHost == "" && c.Version != ""
}

// inProcess: the controller node of a panel that is not an installed
// container. Any other node always gets the helper, so a source-build
// controller never runs a worker's disk command on its own filesystem.
func (h *diskHost) inProcess(ctx context.Context, nodeID string) bool {
	if h.o.cfg.installedPanel() {
		return false
	}
	ctl, err := h.o.controllerID(ctx)
	return err == nil && ctl == nodeID
}

// Disk implements disk.Host. Every call has its own deadline: a stat on a
// dead network path would otherwise hold the caller's locks for good.
func (h *diskHost) Disk(ctx context.Context, nodeID string, r hostdisk.Request) (hostdisk.Result, error) {
	wait := h.short
	if r.Op == "usage" && r.Sizes {
		wait = h.long
	}
	if h.inProcess(ctx, nodeID) {
		ctx, cancel := context.WithTimeout(ctx, wait)
		defer cancel()
		return hostdisk.Run(ctx, hostdisk.Env{Root: "/"}, r), nil
	}
	// the image is pulled outside the op deadline (a first pull is slow and is
	// not a dead mount) under its own, so a hung registry never holds the
	// caller's locks for good. A node that lost the image since gets one retry.
	for try := 0; ; try++ {
		ref, err := h.pull(ctx, nodeID)
		if err != nil {
			return hostdisk.Result{}, err
		}
		octx, cancel := context.WithTimeout(ctx, wait)
		res, err := h.helper(octx, nodeID, ref, r, wait)
		cancel()
		if err != nil && try == 0 && errors.Is(err, docker.ErrNotFound) {
			h.pulled.Delete(nodeID + " " + ref)
			continue
		}
		return res, err
	}
}

// pull makes sure the helper image is on the node and names it.
func (h *diskHost) pull(ctx context.Context, nodeID string) (string, error) {
	ref, err := h.image(ctx)
	if err != nil {
		return "", err
	}
	if _, done := h.pulled.Load(nodeID + " " + ref); done {
		return ref, nil
	}
	pctx, cancel := context.WithTimeout(ctx, h.pullWait)
	defer cancel()
	if err := h.o.docker.Pull(docker.WithNode(pctx, nodeID), ref, "", io.Discard); err != nil {
		return "", fmt.Errorf("pull %s: %w", ref, err)
	}
	h.pulled.Store(nodeID+" "+ref, true)
	return ref, nil
}

// diskHelperSpec is hostHelperSpec under its own name and role label.
func diskHelperSpec(image, name, deadline, request string) docker.ContainerSpec {
	s := hostHelperSpec(image, "host-run", "disk", request)
	s.Name = name
	s.Labels = map[string]string{installspec.LabelRole: diskHelperRole, labelDiskDeadline: deadline}
	return s
}

func (h *diskHost) helper(ctx context.Context, nodeID, ref string, r hostdisk.Request, wait time.Duration) (hostdisk.Result, error) {
	o := h.o
	nctx := docker.WithNode(ctx, nodeID)
	h.sweep(nctx)
	var b [3]byte
	_, _ = rand.Read(b[:])
	name := diskHelperPrefix + hex.EncodeToString(b[:])
	deadline := strconv.FormatInt(time.Now().Add(wait+diskSlack).Unix(), 10)
	id, err := o.docker.Run(nctx, diskHelperSpec(ref, name, deadline, hostdisk.EncodeRequest(r)))
	// Removed even when the caller gave up, and even when Run failed half way.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(nctx), diskCleanWait)
		defer cancel()
		if id == "" {
			id = name
		}
		_ = o.docker.StopRemove(cctx, id)
	}()
	if err != nil {
		return hostdisk.Result{}, err
	}
	code, err := o.docker.Wait(nctx, id)
	if err != nil {
		return hostdisk.Result{}, fmt.Errorf("disk helper: %w", err)
	}
	out, err := o.docker.Logs(nctx, id, 50)
	if err != nil {
		return hostdisk.Result{}, fmt.Errorf("disk helper: %w", err)
	}
	res, ok := hostdisk.ParseResult([]byte(out))
	if !ok || code != 0 {
		return hostdisk.Result{}, fmt.Errorf("the disk helper crashed (exit %d): %s", code, tail(out, 300))
	}
	return res, nil
}

// sweep removes this node's disk helpers that are past their deadline: a
// panel that died mid-call leaves one behind.
func (h *diskHost) sweep(nctx context.Context) {
	cs, err := h.o.docker.List(nctx, map[string]string{installspec.LabelRole: diskHelperRole})
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, c := range cs {
		if at, err := strconv.ParseInt(c.Labels[labelDiskDeadline], 10, 64); err == nil && at > now {
			continue
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(nctx), diskCleanWait)
		if err := h.o.docker.StopRemove(cctx, c.ID); err != nil {
			slog.Warn("disk helper: leftover not removed", "container", c.Name, "err", err)
		}
		cancel()
	}
}

// sweepBoot is the sweep once at start, for the controller's own Docker. A
// panel with no disk rows has nothing to sweep and makes no Docker call.
func (o *Orchestrator) sweepBoot(ctx context.Context) {
	ds, err := o.store.NodeDisks.List(ctx)
	if err != nil || len(ds) == 0 {
		return
	}
	ctx, err = o.withController(ctx)
	if err == nil {
		o.dh.sweep(ctx)
	}
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return s
}
