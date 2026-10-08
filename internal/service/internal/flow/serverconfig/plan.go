// Package serverconfig is the server config file (stackr-server.yml): Parse
// reads it, Diff compares it with a Live snapshot the orchestrator gathers,
// Export writes the live server as a file. Pure: no store, no clone, no
// enqueue. Mirrors flow/orgconfig; docs/rewrite/tasks/serverconfig.md.
//
// plan.go is the wave 0 contract (the plan's type and the default path);
// file.go, diff.go and export.go are the S3 worker's.
package serverconfig

import "github.com/FyrmForge/stackr/internal/service/internal/planfile"

// DefaultPath is where the server file lives when the binding names no path.
const DefaultPath = "stackr-server.yml"

// Change is one line of a plan, planfile's shape. Removal rows (Optional,
// with a Key) are what the file no longer names; impact lines mark risky
// changes.
type Change = planfile.Change

// Plan is what applying the file would do, in the order apply walks it:
// server params, connector shares, settings and the cascade rung, backup
// dests, domains, routes, orgs, then the ticked removals. Blockers refuse
// the apply; Notes do not. Flat in JSON: the stored row reads the same as
// planfile.Plan.
type Plan struct{ planfile.Plan }

// Summary is the plans list's one line: "2 to add, 1 to change, 1 removal
// to review, needs confirm".
func (p *Plan) Summary() string {
	return p.Plan.Summary(func(kind string) bool {
		switch kind {
		case "org-create", "route", "dest", "domain", "param", "connector-share":
			return true
		}
		return false
	})
}
