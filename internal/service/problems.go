package service

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// Problems: every check reconciles an open-problem row, problem.<key> (the
// alert row's key less "alert."), on each conclusive observation: failing
// keeps it, healthy deletes it. The alert row only debounces the webhook.
// The admin banner reads them, so a failing backup shows with no webhook
// set. An offline worker is read off its node row, never stored.

const problemPrefix = "problem."

// ProblemsHref is where the banner's "and N more" goes.
const ProblemsHref = "/-/admin/overview"

// Problem is one open problem. Link and LinkText are where it is fixed.
type Problem struct {
	Key      string
	Severity string // SevCritical or SevWarning
	Message  string // one line
	Since    time.Time
	Link     string
	LinkText string
}

// problemRow is a problem.<key> row's value. The link is built on read
// (problemLink), so a moved page never leaves a stale one in a row.
type problemRow struct {
	Severity string    `json:"severity"`
	Message  string    `json:"message"`
	Since    time.Time `json:"since"` // RFC3339
}

func problemKey(alertKey string) string {
	return problemPrefix + strings.TrimPrefix(alertKey, "alert.")
}

// problemSince is when the problem behind alertKey opened; now if it is not open.
func (o *Orchestrator) problemSince(ctx context.Context, alertKey string) time.Time {
	if v, ok, err := o.store.Settings.Get(ctx, problemKey(alertKey)); err == nil && ok {
		var r problemRow
		if json.Unmarshal([]byte(v), &r) == nil && !r.Since.IsZero() {
			return r.Since
		}
	}
	return time.Now().UTC()
}

// openProblem opens (or rewords) the problem behind alertKey. An open one
// keeps its since; an unchanged one is not written again.
func (o *Orchestrator) openProblem(ctx context.Context, alertKey, sev, msg string) {
	old, _, _ := o.store.Settings.Get(ctx, problemKey(alertKey))
	b, _ := json.Marshal(problemRow{Severity: sev, Message: oneLine(msg), Since: o.problemSince(ctx, alertKey)})
	if string(b) == old {
		return
	}
	if err := o.store.Settings.Set(ctx, problemKey(alertKey), string(b)); err != nil {
		slog.Warn("problems: not kept", "key", alertKey, "err", err)
	}
}

func (o *Orchestrator) hasProblem(ctx context.Context, alertKey string) bool {
	_, ok, err := o.store.Settings.Get(ctx, problemKey(alertKey))
	return err == nil && ok
}

// clearProblem deletes the problem behind alertKey; a healthy check runs
// it every time, so it only writes when there is one.
func (o *Orchestrator) clearProblem(ctx context.Context, alertKey string) {
	if o.hasProblem(ctx, alertKey) {
		_ = o.store.Settings.Delete(ctx, problemKey(alertKey))
	}
}

// sweep deletes the alert and problem rows under alert.<kind> whose rest
// keep says no to; only after a read that saw every subject.
func (o *Orchestrator) sweep(ctx context.Context, kind string, keep func(rest string) bool) {
	for _, prefix := range []string{"alert." + kind, problemPrefix + kind} {
		rows, err := o.store.Settings.ListPrefix(ctx, prefix)
		if err != nil {
			continue
		}
		for k := range rows {
			if rest := strings.TrimPrefix(k, prefix); !keep(rest) {
				_ = o.store.Settings.Delete(ctx, "alert."+kind+rest)
				o.clearProblem(ctx, "alert."+kind+rest)
			}
		}
	}
}

// Problems (admin) is every open problem, critical first, then oldest first.
// A row whose node, app or backup schedule is gone is deleted, with its
// alert row.
func (o *Orchestrator) Problems(ctx context.Context) ([]Problem, error) {
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return nil, err
	}
	nodes := map[string]bool{}
	var out []Problem
	for _, n := range ns {
		nodes[n.ID] = true
		if n.Role == node.Worker && n.Status == node.StatusOffline {
			p := Problem{Key: "node." + n.ID, Severity: SevCritical, Message: o.nodeDown(ctx, n.ID).Detail() + ".",
				Since: o.lastSeen(n.ID)}
			if p.Since.IsZero() {
				p.Since = n.CreatedAt
			}
			p.Link, p.LinkText, _ = o.problemLink(ctx, p.Key, nodes)
			out = append(out, p)
		}
	}
	rows, err := o.store.Settings.ListPrefix(ctx, problemPrefix)
	if err != nil {
		return nil, err
	}
	for k, v := range rows {
		var r problemRow
		if json.Unmarshal([]byte(v), &r) != nil {
			continue
		}
		p := Problem{Key: strings.TrimPrefix(k, problemPrefix), Severity: r.Severity, Message: r.Message, Since: r.Since}
		var ok bool
		if p.Link, p.LinkText, ok = o.problemLink(ctx, p.Key, nodes); ok {
			out = append(out, p)
			continue
		}
		_ = o.store.Settings.Delete(ctx, k)
		_ = o.store.Settings.Delete(ctx, "alert."+p.Key)
	}
	slices.SortFunc(out, func(a, b Problem) int {
		if c := cmp.Compare(sevRank(b.Severity), sevRank(a.Severity)); c != 0 {
			return c
		}
		return cmp.Or(a.Since.Compare(b.Since), cmp.Compare(a.Key, b.Key))
	})
	return out, nil
}

func sevRank(s string) int {
	if s == SevCritical {
		return 1
	}
	return 0
}

// problemLink is the one place a problem's key becomes where it is fixed.
// ok is false when the node, app or schedule it is about is gone.
func (o *Orchestrator) problemLink(ctx context.Context, key string, nodes map[string]bool) (href, text string, ok bool) {
	kind, rest, _ := strings.Cut(key, ".")
	onNode := func(id string) (string, string, bool) {
		return "?drawer=node:" + id, "Open node", nodes[id]
	}
	switch kind {
	case "backup": // backup.panel, backup.schedule.<id>
		id, ok := strings.CutPrefix(rest, "schedule.")
		if !ok {
			return "/-/admin/backups", "Open backups", true
		}
		return o.scheduleLink(ctx, id)
	case "node", "disk", "clock", "mem":
		return onNode(rest)
	case "certfail": // certfail.<node>.<host>
		id, _, _ := strings.Cut(rest, ".")
		return onNode(id)
	case "cert": // cert.root, cert.controller, cert.node.<id>
		if id, isNode := strings.CutPrefix(rest, "node."); isNode {
			return onNode(id)
		}
		return "/-/admin/keys", "Open keys", true
	case "observe":
		return "/-/admin/metrics", "Open metrics and logs", true
	case "crash", "down":
		t, err := o.store.Tiles.Get(ctx, rest)
		if errors.Is(err, errs.ErrNotFound) {
			return "", "", false
		}
		// the tile URL itself is a placeholder; its drawer opens over the env canvas
		return "/" + o.scopePath(ctx, ScopeEnv, t.EnvironmentID) + "?drawer=" + rest + "&tab=status", "Open app", true
	}
	return "/-/admin/alerts", "Open alerts", true
}

// scheduleLink is a volume schedule's problem link: the Storage row of the
// first tile that mounts the volume, else its env. ok is false once the
// schedule or its volume is gone.
func (o *Orchestrator) scheduleLink(ctx context.Context, id string) (href, text string, ok bool) {
	s, err := o.backups.GetSchedule(ctx, id)
	if errors.Is(err, errs.ErrNotFound) {
		return "", "", false
	}
	v, err := o.volumes.Get(ctx, s.VolumeID)
	if errors.Is(err, errs.ErrNotFound) {
		return "", "", false
	}
	if ts, _ := o.mounters(ctx, v); len(ts) > 0 {
		// the tile URL itself is a placeholder; its drawer opens over the env canvas
		return "/" + o.scopePath(ctx, ScopeEnv, ts[0].EnvironmentID) + "?drawer=" + ts[0].ID + "&tab=settings&open=storage", "Open storage", true
	}
	return "/" + o.scopePath(ctx, v.ScopeKind, v.ScopeID), "Open env", true
}

// downAlertKey is the problem of a tile whose deploy failed and left it with
// no container: after a restore that lost its image, or a first deploy that
// never came up. A deploy that works or a stop clears it; a deleted tile
// drops it (problemLink).
func downAlertKey(tileID string) string { return "alert.down." + tileID }

// downAfterFailedDeploy opens the down problem when a deploy failed for real
// (not parked on approval or an unset param, not cancelled) and the tile has
// no container left. A failed deploy with the old container still up is the
// canvas's "degraded", not a banner line.
func (o *Orchestrator) downAfterFailedDeploy(ctx context.Context, tileID string, err error) {
	_, unset := errs.IsUnset(err)
	_, approval := errs.IsNeedsApproval(err)
	if unset || approval || ctx.Err() != nil {
		return
	}
	t, terr := o.store.Tiles.Get(ctx, tileID)
	if terr != nil || t.Kind == tile.Slice || tile.RunToCompletion(t.Kind) {
		return // no containers to be down
	}
	if cs, rerr := o.tiles.Replicas(ctx, t); rerr != nil || len(cs) > 0 {
		return
	}
	env := ""
	if e, eerr := o.store.Environments.Get(ctx, t.EnvironmentID); eerr == nil {
		env = " in " + e.Name
	}
	o.openProblem(ctx, downAlertKey(tileID), SevCritical, t.Name+env+" is down: its deploy failed: "+err.Error())
}
