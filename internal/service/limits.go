package service

import (
	"context"
	"fmt"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/orgconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
)

// checkLimits refuses a cpu or memory limit the Docker host cannot give
// (0 = unlimited, always fine). A container created above the host's size
// fails to create, and a cascade change would send every tile there. The
// message names the host's limit; the fields name the caller's keys.
// ponytail: an unreachable daemon or one that reports no size skips the
// check; the deploy then fails on its own with Docker's words.
func (o *Orchestrator) checkLimits(ctx context.Context, cpu float64, memMB int, cpuField, memField string) error {
	if cpu <= 0 && memMB <= 0 {
		return nil
	}
	h, err := o.docker.HostInfo(ctx)
	if err != nil {
		return nil
	}
	if cpu > 0 && h.CPUs > 0 && cpu > float64(h.CPUs) {
		return errs.Invalidf(cpuField, "This host has %s; the limit can be at most %d.", plural(h.CPUs, "CPU"), h.CPUs)
	}
	if hostMB := int(h.MemBytes >> 20); memMB > 0 && hostMB > 0 && memMB > hostMB {
		return errs.Invalidf(memField, "This host has %d MB of memory; the limit can be at most %d.", hostMB, hostMB)
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
	return o.checkLimits(ctx, cpu, mem, "cpu_limit", "mem_limit_mb")
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
