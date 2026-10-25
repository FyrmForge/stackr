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

// Problems: every alert trigger that fires also keeps an open-problem row,
// problem.<key> (the alert row's key less "alert."), and its clear deletes
// it; the admin banner reads them, so a failing backup shows with no webhook
// set. An offline worker is read off its node row, never stored.

const problemPrefix = "problem."

// ProblemsHref is where the banner's "and N more" goes.
const ProblemsHref = "?drawer=admin&tab=backups"

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
// keeps its since.
func (o *Orchestrator) openProblem(ctx context.Context, alertKey, sev, msg string) {
	b, _ := json.Marshal(problemRow{Severity: sev, Message: oneLine(msg), Since: o.problemSince(ctx, alertKey)})
	if err := o.store.Settings.Set(ctx, problemKey(alertKey), string(b)); err != nil {
		slog.Warn("problems: not kept", "key", alertKey, "err", err)
	}
}

func (o *Orchestrator) clearProblem(ctx context.Context, alertKey string) {
	_ = o.store.Settings.Delete(ctx, problemKey(alertKey))
}

// Problems (admin) is every open problem, critical first, then oldest first.
// A row whose node or app is gone is left out.
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
		}
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
// ok is false when the node or app it is about is gone.
func (o *Orchestrator) problemLink(ctx context.Context, key string, nodes map[string]bool) (href, text string, ok bool) {
	kind, rest, _ := strings.Cut(key, ".")
	onNode := func(id string) (string, string, bool) {
		return "?drawer=node:" + id, "Open node", nodes[id]
	}
	switch kind {
	case "backup":
		return "?drawer=admin&tab=backups", "Open backups", true
	case "node", "disk", "clock", "mem":
		return onNode(rest)
	case "certfail": // certfail.<node>.<host>
		id, _, _ := strings.Cut(rest, ".")
		return onNode(id)
	case "cert": // cert.root, cert.controller, cert.node.<id>
		if id, isNode := strings.CutPrefix(rest, "node."); isNode {
			return onNode(id)
		}
		return "?drawer=admin&tab=nodes", "Open nodes", true
	case "observe":
		return "?drawer=admin&tab=observability", "Open observability", true
	case "crash":
		if _, err := o.store.Tiles.Get(ctx, rest); errors.Is(err, errs.ErrNotFound) {
			return "", "", false
		}
		return "/" + o.scopePath(ctx, ScopeTile, rest), "Open app", true
	}
	return "?drawer=admin&tab=alerts", "Open alerts", true
}
