package env

import (
	"strconv"
	"strings"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

const sep = " · "

// Below is how the Deploys row names the rung below.
func Below(name string) string {
	if name == "" {
		return "the rung below"
	}
	return name
}

// tierLine is a tiered env's lock, which the org's tier decides.
func tierLine(l LockView) string {
	if l.Locked {
		return l.Name + " is a locked tier."
	}
	return l.Name + " is an unlocked tier."
}

// GeneralSummary: "production · rose · locked (org tier)".
func GeneralSummary(name, color string, l LockView) string {
	parts := []string{name}
	if color != "" {
		parts = append(parts, color)
	}
	if l.Show {
		lock := "unlocked"
		if l.Locked {
			lock = "locked"
		}
		if l.Tiered {
			lock += " (org tier)"
		}
		parts = append(parts, lock)
	}
	return strings.Join(parts, sep)
}

// DeploysSummary: "promoted from staging" or "branch main · every push".
func DeploysSummary(from, branch string, auto bool, below string) string {
	if from == "promote" {
		return "promoted from " + Below(below)
	}
	b := "the default branch"
	if branch != "" {
		b = "branch " + branch
	}
	if auto {
		return b + sep + "every push"
	}
	return b + sep + "by hand"
}

// ResourcesSummary is the limits and protection in effect, each inherited
// one with where it comes from: "2 cores (from org) · 2048 MB · protect off".
func ResourcesSummary(rows []c.SettingRowView) string {
	var parts []string
	for _, r := range rows {
		var val string
		switch r.Key {
		case "cpu_limit", "mem_limit_mb":
			val = limit(r.Key, r.Value)
			if r.Value == "" {
				val = r.Effective
			}
			if val == "no limit" || val == "" {
				val = map[string]string{"cpu_limit": "no CPU limit", "mem_limit_mb": "no memory limit"}[r.Key]
			}
		case "protect":
			on := r.Value
			if on == "" {
				on = r.Effective
			}
			val = "protect off"
			if b, _ := strconv.ParseBool(on); b {
				val = "protect on"
			}
		default:
			continue
		}
		if r.From != "" {
			val += " (from " + r.From + ")"
		}
		parts = append(parts, val)
	}
	return strings.Join(parts, sep)
}

func limit(key, v string) string {
	switch {
	case v == "0":
		return "no limit"
	case key == "cpu_limit" && v == "1":
		return "1 core"
	case key == "cpu_limit":
		return v + " cores"
	}
	return v + " MB"
}


// PlacementSummary: "runs on h4 · ingress h4 · exits via h3". exit "" is
// the env's own uplink.
func PlacementSummary(runsOn, ingress, exit string) string {
	var parts []string
	if runsOn != "" {
		parts = append(parts, "runs on "+runsOn)
	}
	if ingress != "" {
		parts = append(parts, "ingress "+ingress)
	}
	if exit == "" {
		parts = append(parts, "own uplink")
	} else {
		parts = append(parts, "exits via "+exit)
	}
	return strings.Join(parts, sep)
}
