// Package hostgrant owns host_grants: the elevated access a server admin
// approved for a stack. A stack's wanted access is the per tile permission
// lines of its tiles (Set); the row is the approved Set. Which lines a tile
// has comes in as a Set (the flows parse them), so this leaf reads no other
// leaf.
package hostgrant

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Permission kinds: what follows the tile slug in a line. A host mount
// perm starts "host:".
const (
	Host = "host:"
	// Device marks a device perm; the rest is the tile's device line.
	Device = "device:"
	// Privileged is the whole perm of the privileged flag.
	Privileged = "privileged"
	// LAN marks a LAN perm: "lan:<ip|cidr>[:port]", the range masked.
	LAN = "lan:"
	// Port marks a published server port: "port:<hostport>[/udp]".
	Port = "port:"
	// NetworkHost is the perm of the host network flag.
	NetworkHost = "network:host"
	// Prefix starts the text of a NeedsApproval for elevated access.
	Prefix = "elevated access: "
)

// Set is the elevated access a stack uses or was granted. Lines is sorted
// and unique: "<tile-slug> <perm>" (see Line).
type Set struct {
	Lines []string
}

// Line is the grant line of a tile's permission.
func Line(slug, perm string) string { return slug + " " + perm }

// Split is Line backwards. A line with no perm gives an empty perm.
func Split(line string) (slug, perm string) {
	slug, perm, _ = strings.Cut(line, " ")
	return slug, perm
}

// Norm sorts and dedupes the lines.
func (s Set) Norm() Set {
	l := slices.Clone(s.Lines)
	slices.Sort(l)
	s.Lines = slices.Compact(l)
	return s
}

// Has reports whether the set holds the line.
func (s Set) Has(line string) bool { return slices.Contains(s.Lines, line) }

// ForTile is the lines of one tile (slug included), in order.
func (s Set) ForTile(slug string) Set {
	var out Set
	for _, l := range s.Lines {
		if sl, _ := Split(l); sl == slug {
			out.Lines = append(out.Lines, l)
		}
	}
	return out
}

// Empty is no access at all.
func (s Set) Empty() bool { return len(s.Lines) == 0 }

// Missing is what s asks for that have does not grant. Only additions need
// approval; ponytail: dropping a line never parks, and a re-added one needs
// the row revoked to be asked again.
func (s Set) Missing(have Set) []string {
	var out []string
	for _, l := range s.Lines {
		if !have.Has(l) {
			out = append(out, l)
		}
	}
	return out
}

// Union is both sets' access.
func (s Set) Union(o Set) Set {
	return Set{Lines: slices.Concat(s.Lines, o.Lines)}.Norm()
}

// Text is the NeedsApproval text for the missing access.
// ponytail: items join with ", "; a line holding ", " would split wrong in
// Parse (paths with it are not worth a quoting scheme).
func Text(missing []string) string { return Prefix + strings.Join(missing, ", ") }

// Parse reads what Text wrote back into a Set; ok is false for any other
// text.
func Parse(what string) (Set, bool) {
	rest, ok := strings.CutPrefix(what, Prefix)
	if !ok {
		return Set{}, false
	}
	var s Set
	for _, it := range strings.Split(rest, ", ") {
		if it != "" {
			s.Lines = append(s.Lines, it)
		}
	}
	return s.Norm(), true
}

type Leaf struct{ rows store.HostGrantStore }

func New(rows store.HostGrantStore) *Leaf { return &Leaf{rows: rows} }

// Row is the stack's grant row; errs.ErrNotFound when there is none.
func (l *Leaf) Row(ctx context.Context, stackID string) (store.HostGrant, error) {
	return l.rows.GetByStack(ctx, stackID)
}

// Of is the stack's approved set; empty when there is no row.
func (l *Leaf) Of(ctx context.Context, stackID string) (Set, error) {
	g, err := l.rows.GetByStack(ctx, stackID)
	if errors.Is(err, errs.ErrNotFound) {
		return Set{}, nil
	}
	return Set{Lines: lines(g.Lines)}.Norm(), err
}

// Check is nil when want is inside the stack's grant, else a NeedsApproval
// naming what is missing.
func (l *Leaf) Check(ctx context.Context, stackID string, want Set) error {
	have, err := l.Of(ctx, stackID)
	if err != nil {
		return err
	}
	if m := want.Missing(have); len(m) > 0 {
		return errs.NeedsApproval{Stack: stackID, What: Text(m)}
	}
	return nil
}

// Approve adds ask to the stack's grant (the row is the union, so another
// env's lines survive) and records who approved it.
func (l *Leaf) Approve(ctx context.Context, stackID, userID string, ask Set) (store.HostGrant, error) {
	g, err := l.rows.GetByStack(ctx, stackID)
	fresh := errors.Is(err, errs.ErrNotFound)
	if fresh {
		g = store.HostGrant{ID: uuid.NewString(), StackID: stackID, CreatedAt: time.Now().UTC()}
	} else if err != nil {
		return g, err
	}
	u := Set{Lines: lines(g.Lines)}.Union(ask)
	g.Lines, g.ApprovedBy = strings.Join(u.Lines, "\n"), userID
	if fresh {
		return g, l.rows.Create(ctx, g)
	}
	return g, l.rows.Update(ctx, g)
}

// Revoke removes the tile's lines, or the whole row when tileSlug is "" or
// nothing is left. Running tiles stay; the next deploy asks again.
func (l *Leaf) Revoke(ctx context.Context, stackID, tileSlug string) error {
	g, err := l.rows.GetByStack(ctx, stackID)
	if err != nil {
		return err
	}
	var keep []string
	if tileSlug != "" {
		for _, ln := range lines(g.Lines) {
			if sl, _ := Split(ln); sl != tileSlug {
				keep = append(keep, ln)
			}
		}
	}
	if len(keep) == 0 {
		return l.rows.Delete(ctx, g.ID)
	}
	g.Lines = strings.Join(keep, "\n")
	return l.rows.Update(ctx, g)
}

// Retain cuts the tile's lines down to keep in one write and reports whether
// anything changed; the row (with its approver and age) stays, and goes only
// when nothing is left. A stack with no row is a no-op.
func (l *Leaf) Retain(ctx context.Context, stackID, tileSlug string, keep []string) (bool, error) {
	g, err := l.rows.GetByStack(ctx, stackID)
	if errors.Is(err, errs.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var next []string
	for _, ln := range lines(g.Lines) {
		if sl, _ := Split(ln); sl != tileSlug || slices.Contains(keep, ln) {
			next = append(next, ln)
		}
	}
	if len(next) == len(lines(g.Lines)) {
		return false, nil
	}
	if len(next) == 0 {
		return true, l.rows.Delete(ctx, g.ID)
	}
	g.Lines = strings.Join(next, "\n")
	return true, l.rows.Update(ctx, g)
}

// All is every stack's grant row, for the admin list.
func (l *Leaf) All(ctx context.Context) ([]store.HostGrant, error) { return l.rows.List(ctx) }

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
