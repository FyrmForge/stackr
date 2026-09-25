// Package environment owns environments: the row and its Docker network. It
// owns the ladder (position: "is X above Y", who promotes from whom), the
// from/auto knobs and base_env for PR envs. Clone and teardown are flows;
// the rules they take from here are CloneRow, Reclaim and Rewrite.
package environment

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	Static    = "static"
	Ephemeral = "ephemeral" // PR envs: made and removed by the connector

	FromBranch  = "branch"
	FromPromote = "promote" // releases arrive only from the env below
)

// Networks is the slice of the Docker wrapper this leaf needs.
type Networks interface {
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) error
	RemoveNetwork(ctx context.Context, name string) error
	Gateways(ctx context.Context, labels map[string]string) ([]string, error)
}

type Leaf struct {
	envs store.EnvironmentStore
	net  Networks
}

func New(envs store.EnvironmentStore, net Networks) *Leaf {
	return &Leaf{envs: envs, net: net}
}

func (l *Leaf) Get(ctx context.Context, id string) (store.Environment, error) {
	return l.envs.Get(ctx, id)
}

func (l *Leaf) GetBySlug(ctx context.Context, stackID, slug string) (store.Environment, error) {
	return l.envs.GetBySlug(ctx, stackID, slug)
}

// IsDefault reports whether e is its stack's default env: the ladder's top
// rung (DECIDE 192). A PR env never is.
func (l *Leaf) IsDefault(ctx context.Context, e store.Environment) (bool, error) {
	es, err := l.Ladder(ctx, e.StackID)
	if err != nil || len(es) == 0 {
		return false, err
	}
	return es[len(es)-1].ID == e.ID, nil
}

// Ladder is the stack's static envs bottom rung first; the last is the
// default env. PR envs are never rungs: nothing promotes from or to them.
func (l *Leaf) Ladder(ctx context.Context, stackID string) ([]store.Environment, error) {
	all, err := l.envs.ListByStack(ctx, stackID)
	es := all[:0]
	for _, e := range all {
		if e.Type == Static {
			es = append(es, e)
		}
	}
	sort.SliceStable(es, func(i, j int) bool { return es[i].Position < es[j].Position })
	return es, err
}

// List is every env of the stack, PR envs included, ladder order first.
func (l *Leaf) List(ctx context.Context, stackID string) ([]store.Environment, error) {
	es, err := l.envs.ListByStack(ctx, stackID)
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].Type != es[j].Type {
			return es[i].Type == Static
		}
		return es[i].Position < es[j].Position
	})
	return es, err
}

// Above reports whether a sits above b on the ladder.
func Above(a, b store.Environment) bool { return a.Position > b.Position }

// Palette is the env hues in the order an uncoloured env takes them (v0's
// envcolor.Names); each is an env-c-<hue> class.
var Palette = []string{"teal", "sky", "lime", "amber", "rose", "violet"}

// bySlug are the hues people expect without a legend: red means production,
// violet (the accent) the env you hack on.
var bySlug = map[string]string{
	"production":  "rose",
	"prod":        "rose",
	"staging":     "amber",
	"stg":         "amber",
	"dev":         "violet",
	"development": "violet",
}

// Hues is the hue every env of one stack is drawn in, by env id; envs is
// List's order. The stored colour wins; well-known slugs claim theirs
// next, then statics before PR envs take the next hue nobody holds,
// cycling past six (v0's envcolor.Map).
func Hues(envs []store.Environment) map[string]string {
	out := make(map[string]string, len(envs))
	used := map[string]bool{}
	for _, e := range envs {
		if h, ok := bySlug[e.Slug]; ok && e.Type != Ephemeral {
			out[e.ID] = h
			used[h] = true
		}
	}
	next := 0
	for pass := range 2 {
		for _, e := range envs {
			if (e.Type == Ephemeral) != (pass == 1) || out[e.ID] != "" {
				continue
			}
			h := Palette[next%len(Palette)]
			for tries := 0; tries < len(Palette) && used[h]; tries++ {
				next++
				h = Palette[next%len(Palette)]
			}
			next++
			used[h] = true
			out[e.ID] = h
		}
	}
	for _, e := range envs {
		if e.Color != "" {
			out[e.ID] = e.Color
		}
	}
	return out
}

// Below is the env a `from: promote` env promotes from: the next rung down.
// ErrNotFound on the bottom rung and for a PR env.
func (l *Leaf) Below(ctx context.Context, e store.Environment) (store.Environment, error) {
	if e.Type != Static {
		return store.Environment{}, errs.ErrNotFound
	}
	es, err := l.Ladder(ctx, e.StackID)
	if err != nil {
		return store.Environment{}, err
	}
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Position < e.Position {
			return es[i], nil
		}
	}
	return store.Environment{}, errs.ErrNotFound
}

// Spec is what Create needs beyond the name. Base is a PR env's base env id.
type Spec struct {
	Type       string
	Base       *string
	Color      string
	FromKind   string
	FromBranch string
	Auto       bool
}

// Create puts a static env on top of the ladder; a PR env takes the same
// number but is never on it. Its network is made on the first
// Network call, not here.
func (l *Leaf) Create(ctx context.Context, stackID, name string, sp Spec) (store.Environment, error) {
	ladder, err := l.Ladder(ctx, stackID)
	if err != nil {
		return store.Environment{}, err
	}
	pos := 0
	if len(ladder) > 0 {
		pos = ladder[len(ladder)-1].Position + 1
	}
	if sp.Type != Static && sp.Type != Ephemeral {
		return store.Environment{}, errs.Invalidf("type", "Unknown environment type %q.", sp.Type)
	}
	id := uuid.NewString()
	e := store.Environment{
		ID:        id,
		StackID:   stackID,
		Type:      sp.Type,
		BaseEnvID: sp.Base,
		Settings:  "{}",
		Color:     sp.Color,
		Position:  pos,
		Network:   "stackr-env-" + id,
		CreatedAt: time.Now().UTC(),
	}
	if err := l.name(ctx, &e, name); err != nil {
		return e, err
	}
	if err := checkFrom(e, sp.FromKind, sp.FromBranch, len(ladder) == 0); err != nil {
		return e, err
	}
	e.FromKind, e.FromBranch, e.Auto = sp.FromKind, sp.FromBranch, sp.Auto
	return e, l.envs.Create(ctx, e)
}

// CloneRow is a PR env made from base: same settings and color, its own
// branch, ephemeral, off the ladder. Tiles, params and slices are the
// flow's (see the envops rules), never this row's.
func (l *Leaf) CloneRow(ctx context.Context, base store.Environment, name, branch string) (store.Environment, error) {
	e, err := l.Create(ctx, base.StackID, name, Spec{
		Type:       Ephemeral,
		Base:       &base.ID,
		Color:      base.Color,
		FromKind:   FromBranch,
		FromBranch: branch,
		Auto:       true,
	})
	if err != nil {
		return e, err
	}
	return l.SetSettings(ctx, e, base.Settings)
}

func (l *Leaf) Rename(ctx context.Context, e store.Environment, name string) (store.Environment, error) {
	if err := l.name(ctx, &e, name); err != nil {
		return e, err
	}
	return e, l.envs.Update(ctx, e)
}

func (l *Leaf) name(ctx context.Context, e *store.Environment, name string) error {
	name = strings.TrimSpace(name)
	sl := slug.Make(name)
	switch {
	case name == "":
		return errs.Invalidf("name", "Give the environment a name.")
	case sl == "":
		return errs.Invalidf("name", "That name needs at least one letter or digit, since it becomes the URL.")
	case slug.Reserved(sl):
		return errs.Invalidf("name", "%q is reserved.", sl)
	}
	if other, err := l.envs.GetBySlug(ctx, e.StackID, sl); err == nil && other.ID != e.ID {
		return errs.Invalidf("name", "Another environment in this stack already uses that name.")
	} else if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return err
	}
	e.Name, e.Slug = name, sl
	return nil
}

func checkFrom(e store.Environment, kind, branch string, bottom bool) error {
	switch {
	case kind == FromBranch && strings.TrimSpace(branch) == "":
		return errs.Invalidf("from", "Name the branch this environment builds from.")
	case kind == FromPromote && bottom:
		return errs.Invalidf("from", "The bottom environment has nothing below it to promote from; give it a branch.")
	case kind == FromPromote && e.Type == Ephemeral:
		return errs.Invalidf("from", "A PR environment builds from its branch.")
	case kind != FromBranch && kind != FromPromote:
		return errs.Invalidf("from", "Unknown source %q.", kind)
	}
	return nil
}

// SetFrom sets the two knobs. promote drops the branch.
func (l *Leaf) SetFrom(
	ctx context.Context,
	e store.Environment,
	kind, branch string,
	auto bool,
) (store.Environment, error) {
	if kind == FromPromote {
		branch = ""
	}
	_, err := l.Below(ctx, e)
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return e, err
	}
	if err := checkFrom(e, kind, branch, err != nil); err != nil {
		return e, err
	}
	e.FromKind, e.FromBranch, e.Auto = kind, branch, auto
	return e, l.envs.Update(ctx, e)
}

// Reorder rewrites positions: ids bottom rung first, every static env of the
// stack exactly once. The new bottom rung must build from a branch.
func (l *Leaf) Reorder(ctx context.Context, stackID string, ids []string) error {
	es, err := l.Ladder(ctx, stackID)
	if err != nil {
		return err
	}
	byID := make(map[string]store.Environment, len(es))
	for _, e := range es {
		byID[e.ID] = e
	}
	if len(ids) != len(es) {
		return errs.Invalidf("order", "List every environment of the stack once.")
	}
	for i, id := range ids {
		e, ok := byID[id]
		if !ok {
			return errs.Invalidf("order", "List every environment of the stack once.")
		}
		if i == 0 && e.FromKind == FromPromote {
			return errs.Invalidf("order", "%s promotes from the env below it, so it cannot be the bottom rung.", e.Name)
		}
		delete(byID, id)
		e.Position = i
		es[i] = e
	}
	// ponytail: one update per row outside a tx; the caller wraps it in
	// store.Tx when a half-applied order matters.
	for _, e := range es {
		if err := l.envs.Update(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// SetRelease records what the env runs now.
func (l *Leaf) SetRelease(ctx context.Context, e store.Environment, releaseID string) (store.Environment, error) {
	e.ReleaseID = &releaseID
	return e, l.envs.Update(ctx, e)
}

// SetSettings stores the defaults cascade slot as given (leaf/settings merges).
func (l *Leaf) SetSettings(ctx context.Context, e store.Environment, blob string) (store.Environment, error) {
	e.Settings = blob
	return e, l.envs.Update(ctx, e)
}

func (l *Leaf) SetColor(ctx context.Context, e store.Environment, color string) (store.Environment, error) {
	e.Color = color
	return e, l.envs.Update(ctx, e)
}

// Network ensures the env's Docker network and returns its name.
func (l *Leaf) Network(ctx context.Context, e store.Environment) (string, error) {
	return e.Network, l.net.EnsureNetwork(ctx, e.Network, map[string]string{"stackr.env": e.ID})
}

// Gateways are the gateway IPs of every stackr network (env, shared,
// ingress): where the host, and so the host-network panel, meets them.
func (l *Leaf) Gateways(ctx context.Context) ([]string, error) {
	return l.net.Gateways(ctx, map[string]string{docker.LabelManaged: "true"})
}

// Shared ensures a network that crosses envs (a stack- or org-scoped
// managed instance's); the name is the caller's.
func (l *Leaf) Shared(ctx context.Context, name string) error {
	return l.net.EnsureNetwork(ctx, name, map[string]string{"stackr.shared": "true"})
}

// Delete refuses while the env has tiles; the teardown flow empties it
// first. The network goes before the row, so a failure leaves a row to retry
// the delete from.
// ponytail: deleting the bottom rung can leave a promote env at the bottom;
// its Below then answers ErrNotFound and promote refuses. Guard here if that
// confuses anyone.
func (l *Leaf) Delete(ctx context.Context, e store.Environment, tileCount int) error {
	if tileCount > 0 {
		return errs.Conflictf("Remove this environment's tiles first.")
	}
	if err := l.net.RemoveNetwork(ctx, e.Network); err != nil {
		return err
	}
	return l.envs.Delete(ctx, e.ID)
}

// Reclaim is what happens to a provisioned slice when its env goes: an
// ephemeral env's are dropped (nothing else would ever reclaim them), a
// static env's detach and keep the data.
func Reclaim(envType string) string {
	if envType == Ephemeral {
		return "drop"
	}
	return "detach"
}

// Rewrite replaces every base db password with the clone's new one, in a
// tile's env blob or a copied param value. The clone mints every password
// first, then rewrites, or a tile copied before its database keeps the old one.
func Rewrite(s string, passwords map[string]string) string {
	for old, fresh := range passwords {
		s = strings.ReplaceAll(s, old, fresh)
	}
	return s
}
