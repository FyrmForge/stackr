package service

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
)

// pgextupdKey is an instance's extension update check: pgextupd.<instance id>,
// JSON of PGExtUpdates. No row = never checked.
const pgextupdKey = "pgextupd."

// PGExtChange is one package a newer build exists for: From is what the
// instance has, To what a rebuild would install.
type PGExtChange struct {
	Package string `json:"package"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// PGExtUpdates is the last update check of a recipe-built instance. Empty
// Changes = up to date. Ref is the build the check read: a check of an older
// build is never shown.
type PGExtUpdates struct {
	CheckedAt time.Time     `json:"checked_at"`
	Ref       string        `json:"ref,omitempty"`
	Changes   []PGExtChange `json:"changes"`
}

// PGExtUpdatesOf is the pending extension updates of an instance: nil when
// none were found, it was never checked, or the build moved since the check.
func (o *Orchestrator) PGExtUpdatesOf(ctx context.Context, m OperatedInstance) *PGExtUpdates {
	v, ok, err := o.store.Settings.Get(ctx, pgextupdKey+m.ID)
	if err != nil || !ok {
		return nil
	}
	var u PGExtUpdates
	if json.Unmarshal([]byte(v), &u) != nil || len(u.Changes) == 0 {
		return nil
	}
	if eb, _ := mflow.ParseExtBuild(m.ExtBuild); !eb.Built() || eb.Ref != u.Ref {
		return nil
	}
	return &u
}

// PGExtUpdatesFor is PGExtUpdatesOf by tile id, for the drawer's stream.
func (o *Orchestrator) PGExtUpdatesFor(ctx context.Context, tileID string) *PGExtUpdates {
	if _, m, err := o.instanceOf(ctx, tileID); err == nil {
		return o.PGExtUpdatesOf(ctx, m)
	}
	return nil
}

// pgExtUpdateCount is how many packages have an update, for the canvas card.
func (o *Orchestrator) pgExtUpdateCount(ctx context.Context, m OperatedInstance) int {
	if u := o.PGExtUpdatesOf(ctx, m); u != nil {
		return len(u.Changes)
	}
	return 0
}

func (o *Orchestrator) delPGExtUpdates(ctx context.Context, instanceID string) {
	if err := o.store.Settings.Delete(ctx, pgextupdKey+instanceID); err != nil {
		slog.Warn("pg extension updates: not cleared", "instance", instanceID, "err", err)
	}
}

// checkPGExtUpdates is the daily check of every recipe-built instance: the
// resolve step of a plan runs in the instance's base image on its node and
// the newest installable builds are set against the build record. Nothing is
// built and no plan stored. Instances on the same node, base, major and
// package set share one resolve. A failed resolve keeps the last result. Rows
// of instances that are gone or no longer built are dropped.
func (o *Orchestrator) checkPGExtUpdates(ctx context.Context) {
	ms, err := o.operated.WithEngine(ctx, "postgres")
	if err != nil {
		slog.Warn("pg extension updates: list", "err", err)
		return
	}
	live := map[string]bool{}
	shared := map[string]*mflow.Resolved{} // nil = the resolve failed
	for _, m := range ms {
		eb, _ := mflow.ParseExtBuild(m.ExtBuild)
		if !eb.Built() {
			continue
		}
		live[m.ID] = true
		if err := o.checkPGExtUpdate(ctx, m, eb, shared); err != nil {
			slog.Warn("pg extension updates: check", "instance", m.ID, "err", err)
		}
	}
	if err := o.store.Settings.Set(ctx, pgextCheckedKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		slog.Warn("pg extension updates: run time not kept", "err", err)
	}
	rows, err := o.store.Settings.ListPrefix(ctx, pgextupdKey)
	if err != nil {
		return
	}
	for k := range rows {
		if id := strings.TrimPrefix(k, pgextupdKey); !live[id] {
			o.delPGExtUpdates(ctx, id)
		}
	}
}

// pgextCheckedKey is when the update check last ran (RFC 3339).
const pgextCheckedKey = "pgextcheck.last"

// bootPGExtCheck runs the update check once after boot when a day has gone
// by since the last run, so a panel restarted more often than daily still
// checks. The daily ticker runs it from then on.
func (o *Orchestrator) bootPGExtCheck(ctx context.Context) {
	if v, ok, _ := o.store.Settings.Get(ctx, pgextCheckedKey); ok {
		if at, err := time.Parse(time.RFC3339, v); err == nil && time.Since(at) < 24*time.Hour {
			return
		}
	}
	ctx, cancel := context.WithTimeout(ctx, dailyChoresBudget)
	defer cancel()
	o.checkPGExtUpdates(ctx)
}

// debVersionCmp orders two package versions as dpkg does (epoch, upstream,
// revision; letters before digits' runs, "~" before everything): -1, 0, 1.
// The URL recipes' versions (1.2.0, v1.2.0) order the same way.
func debVersionCmp(a, b string) int {
	ea, a := debEpoch(a)
	eb, b := debEpoch(b)
	if ea != eb {
		return cmp.Compare(ea, eb)
	}
	ua, ra := a, ""
	if i := strings.LastIndex(a, "-"); i >= 0 {
		ua, ra = a[:i], a[i+1:]
	}
	ub, rb := b, ""
	if i := strings.LastIndex(b, "-"); i >= 0 {
		ub, rb = b[:i], b[i+1:]
	}
	if c := debPartCmp(ua, ub); c != 0 {
		return c
	}
	return debPartCmp(ra, rb)
}

func debEpoch(v string) (int, string) {
	if e, rest, ok := strings.Cut(v, ":"); ok {
		if n, err := strconv.Atoi(e); err == nil {
			return n, rest
		}
	}
	return 0, v
}

func debPartCmp(a, b string) int {
	digit := func(c byte) bool { return c >= '0' && c <= '9' }
	order := func(s string, i int) int {
		if i >= len(s) || digit(s[i]) {
			return 0
		}
		switch c := s[i]; {
		case c == '~':
			return -1
		case c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z':
			return int(c)
		default:
			return int(c) + 256
		}
	}
	for a != "" || b != "" {
		i, j := 0, 0
		for i < len(a) && !digit(a[i]) || j < len(b) && !digit(b[j]) {
			if c := cmp.Compare(order(a, i), order(b, j)); c != 0 {
				return c
			}
			i, j = i+1, j+1
		}
		a, b = a[min(i, len(a)):], b[min(j, len(b)):]
		i, j = 0, 0
		for i < len(a) && digit(a[i]) {
			i++
		}
		for j < len(b) && digit(b[j]) {
			j++
		}
		na, nb := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
		if c := cmp.Or(cmp.Compare(len(na), len(nb)), strings.Compare(na, nb)); c != 0 {
			return c
		}
		a, b = a[i:], b[j:]
	}
	return 0
}

func (o *Orchestrator) checkPGExtUpdate(ctx context.Context, m OperatedInstance, eb mflow.ExtBuild, shared map[string]*mflow.Resolved) error {
	t, err := o.tiles.Get(ctx, m.TileID)
	if err != nil {
		return err
	}
	node, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	rs, err := o.pgResolve(ctx, t, m, eb.Recipes)
	if err != nil {
		return err
	}
	changes := []PGExtChange{}
	if pkgs := mflow.RecipePackages(rs, m.Major); len(pkgs) > 0 {
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%s", node, eb.Base, m.Major, strings.Join(pkgs, " "))
		res, seen := shared[key]
		if !seen {
			r, err := o.pgResolveOn(ctx, node, eb.Base, m.ID, m.Major, pkgs)
			if err != nil {
				shared[key] = nil
				return err
			}
			res = &r
			shared[key] = res
		}
		if res == nil {
			return fmt.Errorf("resolve on %s failed", eb.Base)
		}
		for _, p := range pkgs {
			to, from := res.Versions[p], eb.Packages[p]
			switch {
			case to == "" || from == "" || to == from:
			case debVersionCmp(to, from) <= 0: // the repository dropped or rolled back the build
				slog.Info("pg extension updates: candidate is not newer", "package", p, "have", from, "candidate", to)
			default:
				changes = append(changes, PGExtChange{Package: p, From: from, To: to})
			}
		}
	}
	// URL packages: the recipe as it is now against the release the build holds
	urls, err := mflow.URLPackages(rs, m.Major, eb.Arch)
	if err != nil {
		return err
	}
	for p, u := range urls {
		old, ok := eb.URLs[p]
		switch {
		case !ok || old.Version == u.Version:
		case debVersionCmp(u.Version, old.Version) <= 0:
			slog.Info("pg extension updates: recipe version is not newer", "package", p, "have", old.Version, "recipe", u.Version)
		default:
			changes = append(changes, PGExtChange{Package: p, From: old.Version, To: u.Version})
		}
	}
	slices.SortFunc(changes, func(a, b PGExtChange) int { return strings.Compare(a.Package, b.Package) })
	b, _ := json.Marshal(PGExtUpdates{CheckedAt: time.Now().UTC(), Ref: eb.Ref, Changes: changes})
	return o.store.Settings.Set(ctx, pgextupdKey+m.ID, string(b))
}
