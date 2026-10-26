package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// checkLimits refuses a cpu or memory limit the node cannot give (0 =
// unlimited, always fine). A container created above the host's size fails
// to create, and a cascade change would send every tile there. The message
// names the host's limit; the fields name the caller's keys. A tile's limit
// is checked against its env's node.
// ponytail: an unreachable daemon or one that reports no size skips the
// check; the deploy then fails on its own with Docker's words.
func (o *Orchestrator) checkLimits(
	ctx context.Context,
	n store.Node,
	cpu float64,
	memMB int,
	cpuField, memField string,
) error {
	return o.checkLimitsOn(ctx, []store.Node{n}, cpu, memMB, cpuField, memField)
}

// checkLimitsOn checks against the smallest of ns, CPU and memory each on
// its own: both refusals at once, each naming the node that decides it.
func (o *Orchestrator) checkLimitsOn(
	ctx context.Context,
	ns []store.Node,
	cpu float64,
	memMB int,
	cpuField, memField string,
) error {
	if cpu <= 0 && memMB <= 0 {
		return nil
	}
	name := func(n store.Node) string {
		if n.Role == node.Controller {
			return "This host"
		}
		return "Node " + n.Name
	}
	var cpuAt, memAt string
	minCPU, minMB := 0, 0
	for _, n := range ns {
		h, err := o.docker.HostInfo(docker.WithNode(ctx, n.ID))
		if err != nil {
			continue
		}
		if h.CPUs > 0 && (minCPU == 0 || h.CPUs < minCPU) {
			minCPU, cpuAt = h.CPUs, name(n)
		}
		if mb := int(h.MemBytes >> 20); mb > 0 && (minMB == 0 || mb < minMB) {
			minMB, memAt = mb, name(n)
		}
	}
	var out []error
	if cpu > 0 && minCPU > 0 && cpu > float64(minCPU) {
		out = append(out, errs.Invalidf(cpuField, "%s has %s; the limit can be at most %d.", cpuAt, plural(minCPU, "CPU"), minCPU))
	}
	if memMB > 0 && minMB > 0 && memMB > minMB {
		out = append(out, errs.Invalidf(memField, "%s has %d MB of memory; the limit can be at most %d.", memAt, minMB, minMB))
	}
	return errors.Join(out...)
}

// checkLimitsEverywhere is checkLimits for a limit that reaches tiles on any
// node (a cascade rung): it must fit every one, the smallest decides.
// ponytail: all nodes, not just the ones the org or stack has envs on; scope
// it when pins and the placement UI make that cheap to ask.
func (o *Orchestrator) checkLimitsEverywhere(
	ctx context.Context,
	cpu float64,
	memMB int,
	cpuField, memField string,
) error {
	if cpu <= 0 && memMB <= 0 {
		return nil
	}
	if _, err := o.controllerID(ctx); err != nil { // a database nobody bootstrapped
		return err
	}
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return err
	}
	return o.checkLimitsOn(ctx, ns, cpu, memMB, cpuField, memField)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// checkSettingLimits is checkLimits for a rung's settings.Settings.
func (o *Orchestrator) checkSettingLimits(ctx context.Context, s settings.Settings) error {
	var cpu float64
	var mem int
	if s.CPULimit != nil {
		cpu = *s.CPULimit
	}
	if s.MemLimitMB != nil {
		mem = *s.MemLimitMB
	}
	var bad []error
	if cpu < 0 {
		bad, cpu = append(bad, errs.Invalidf("cpu_limit", "Use 0 or more; 0 is unlimited.")), 0
	}
	if mem < 0 {
		bad, mem = append(bad, errs.Invalidf("mem_limit_mb", "Use 0 or more; 0 is unlimited.")), 0
	}
	return errors.Join(append(bad, o.checkLimitsEverywhere(ctx, cpu, mem, "cpu_limit", "mem_limit_mb"))...)
}

// checkBlobLimits is checkSettingLimits for a stored blob; one that does not
// parse is the setter's own error to report.
func (o *Orchestrator) checkBlobLimits(ctx context.Context, blob string) error {
	s, err := settings.Parse(blob)
	if err != nil {
		return nil
	}
	return o.checkSettingLimits(ctx, s)
}

// checkValLimits is checkLimits for the server rung's string values.
func (o *Orchestrator) checkValLimits(ctx context.Context, vals map[string]string) error {
	var s settings.Settings
	for _, k := range []string{"cpu_limit", "mem_limit_mb"} {
		if raw, ok := vals[k]; ok {
			if err := settings.Set(&s, k, raw); err != nil {
				return nil // SetDefaults refuses it with its own words
			}
		}
	}
	return o.checkSettingLimits(ctx, s)
}

// limitBlocker is the plan blocker for a file's cascade limits, "" when
// they fit the host.
func (o *Orchestrator) limitBlocker(ctx context.Context, cpu *float64, memMB *int) string {
	err := o.checkSettingLimits(ctx, settings.Settings{CPULimit: cpu, MemLimitMB: memMB})
	if inv, ok := errs.IsInvalid(err); ok {
		return "defaults: " + inv.Msg
	}
	return ""
}

// limitBlock adds a blocker to a server plan whose defaults block asks for
// more than the host has, so the file can never apply what Docker refuses.
func (o *Orchestrator) limitBlock(ctx context.Context, f *serverconfig.File, p serverconfig.Plan) serverconfig.Plan {
	if f != nil && f.Defaults != nil {
		if b := o.limitBlocker(ctx, f.Defaults.CPULimit, f.Defaults.MemLimitMB); b != "" {
			p.Blockers = append(p.Blockers, b)
		}
	}
	return p
}

// orgLimitBlock is limitBlock for an org file.
func (o *Orchestrator) orgLimitBlock(ctx context.Context, f *orgconfig.File, p orgconfig.Plan) orgconfig.Plan {
	if f != nil && f.Defaults != nil {
		if b := o.limitBlocker(ctx, f.Defaults.CPULimit, f.Defaults.MemLimitMB); b != "" {
			p.Blockers = append(p.Blockers, b)
		}
	}
	return p
}

// checkProtect refuses a rung that turns password protection on while no
// user and password reach it (its own, or a level above's): the proxy
// would lock every URL below with a pair nobody knows. above is the levels
// over it, server first; stored is the rung before this save. A rung whose
// stored password went only because its user was cleared is missing the
// user alone: typing one back keeps that password.
func checkProtect(above []settings.Settings, stored, blob string) error {
	s, err := settings.Parse(blob)
	if err != nil || s.Protect == nil || !*s.Protect {
		return nil // a corrupt blob is the setter's own error
	}
	if settings.Resolve(append(above, s)...).ProtectUser != "" {
		return nil
	}
	if old, err := settings.Parse(stored); err == nil && old.ProtectPassword != nil && *old.ProtectPassword != "" &&
		(s.ProtectPassword == nil || *s.ProtectPassword == "") {
		return errs.Invalidf("protect_user", "Protection needs a user as well as a password.")
	}
	const msg = "Protection needs a user and a password, here or on a level above."
	return errors.Join(errs.Invalidf("protect_user", msg), errs.Invalidf("protect_password", msg))
}

// levelsAbove is the cascade over a rung, server first: the server's
// defaults, then each blob given (the org's, the stack's).
func (o *Orchestrator) levelsAbove(ctx context.Context, blobs ...string) ([]settings.Settings, error) {
	d, err := o.settings.Defaults(ctx)
	if err != nil {
		return nil, err
	}
	out := []settings.Settings{d}
	for _, b := range blobs {
		s, err := settings.Parse(b)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
