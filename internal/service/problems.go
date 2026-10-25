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
		if id, ok := strings.CutPrefix(rest, "schedule."); ok {
			if _, err := o.backups.GetSchedule(ctx, id); errors.Is(err, errs.ErrNotFound) {
				return "", "", false
			}
		}
		return "/-/admin/backups", "Open backups", true
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
	case "crash":
		if _, err := o.store.Tiles.Get(ctx, rest); errors.Is(err, errs.ErrNotFound) {
			return "", "", false
		}
		return "/" + o.scopePath(ctx, ScopeTile, rest), "Open app", true
	}
	return "/-/admin/alerts", "Open alerts", true
}
