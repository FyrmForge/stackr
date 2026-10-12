package service

import (
	"context"
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
	if cpu <= 0 && memMB <= 0 {
		return nil
	}
	h, err := o.docker.HostInfo(docker.WithNode(ctx, n.ID))
	if err != nil {
		return nil
	}
	host := "This host"
	if n.Role != node.Controller {
		host = "Node " + n.Name
	}
	if cpu > 0 && h.CPUs > 0 && cpu > float64(h.CPUs) {
		return errs.Invalidf(cpuField, "%s has %s; the limit can be at most %d.", host, plural(h.CPUs, "CPU"), h.CPUs)
	}
	if hostMB := int(h.MemBytes >> 20); memMB > 0 && hostMB > 0 && memMB > hostMB {
		return errs.Invalidf(memField, "%s has %d MB of memory; the limit can be at most %d.", host, hostMB, hostMB)
	}
	return nil
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
	for _, n := range ns {
		if err := o.checkLimits(ctx, n, cpu, memMB, cpuField, memField); err != nil {
			return err
		}
	}
	return nil
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
	return o.checkLimitsEverywhere(ctx, cpu, mem, "cpu_limit", "mem_limit_mb")
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
