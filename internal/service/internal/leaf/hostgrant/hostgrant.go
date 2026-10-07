// Package hostgrant owns host_grants: the host access a server admin
// approved for a stack. A stack's wanted access is the host mount lines,
// device lines and privileged flag of its tiles (Set); the row is the
// approved Set. Which lines a tile has comes in as a Set (the flows parse
// them), so this leaf reads no other leaf.
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

const (
	// Device marks a device line in a Set; a host mount line starts "host:".
	Device = "device:"
	// Privileged is the token a missing privileged flag shows as.
	Privileged = "privileged"
	// Prefix starts the text of a NeedsApproval for host access.
	Prefix = "host access: "
)

// Set is the host access a stack uses or was granted. Lines is sorted and
// unique: "host:/a:/b[:ro]" mounts and "device:<line>" devices.
type Set struct {
	Lines      []string
	Privileged bool
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

// Empty is no access at all.
func (s Set) Empty() bool { return len(s.Lines) == 0 && !s.Privileged }

// Missing is what s asks for that have does not grant: lines, then
// "privileged". Only additions need approval; ponytail: dropping a line
// never parks, and a re-added one needs the row revoked to be asked again.
func (s Set) Missing(have Set) []string {
	var out []string
	for _, l := range s.Lines {
		if !have.Has(l) {
			out = append(out, l)
		}
	}
	if s.Privileged && !have.Privileged {
		out = append(out, Privileged)
	}
	return out
}

// Union is both sets' access.
func (s Set) Union(o Set) Set {
	return Set{Lines: slices.Concat(s.Lines, o.Lines), Privileged: s.Privileged || o.Privileged}.Norm()
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
		if it == Privileged {
			s.Privileged = true
		} else if it != "" {
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
	return Set{Lines: lines(g.Lines), Privileged: g.Privileged}.Norm(), err
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
	have := Set{Lines: lines(g.Lines), Privileged: g.Privileged}
	u := have.Union(ask)
	g.Lines, g.Privileged, g.ApprovedBy = strings.Join(u.Lines, "\n"), u.Privileged, userID
	if fresh {
		return g, l.rows.Create(ctx, g)
	}
	return g, l.rows.Update(ctx, g)
}

// Revoke deletes the row. Running tiles stay; the next deploy asks again.
func (l *Leaf) Revoke(ctx context.Context, stackID string) error {
	g, err := l.rows.GetByStack(ctx, stackID)
	if err != nil {
		return err
	}
	return l.rows.Delete(ctx, g.ID)
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
