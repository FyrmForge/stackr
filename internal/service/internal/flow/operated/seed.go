package operated

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Dump formats Seed loads.
const (
	DumpCustom = "custom" // pg_dump -Fc, through pg_restore
	DumpPlain  = "plain"  // SQL text, through psql
)

// SniffLen is how much of a dump SniffDump wants to see.
const SniffLen = 262

// SniffDump names a dump's format from its first SniffLen bytes. The
// formats it cannot load are refused with what to do instead; anything
// else is taken as plain SQL and psql says if it is not.
func SniffDump(head []byte) (string, error) {
	switch {
	case len(head) == 0:
		return "", errs.Invalidf("dump", "the dump is empty")
	case bytes.HasPrefix(head, []byte("PGDMP")):
		return DumpCustom, nil
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return "", errs.Invalidf("dump", "the dump is gzipped; unpack it first (gunzip -c dump.gz)")
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return "", errs.Invalidf("dump", "tar dumps are not loaded; use pg_dump -Fc or plain SQL")
	}
	return DumpPlain, nil
}

// seeder is an engine that loads a dump into a slice as its owner.
type seeder interface {
	Seed(format string, i Instance, s Slice) []string
}

// sliceDumper is an engine that dumps one slice as DumpCustom.
type sliceDumper interface {
	SliceDump(i Instance, s Slice) []string
}

// DumpSlice streams slice tile s's database as a DumpCustom dump, run in
// the container of the instance it lives on. The caller reads the stream to
// its end and calls wait, which reports the dump's exit (a failed dump
// carries the tail of pg_dump's stderr). Nothing touches disk.
func (f *Flow) DumpSlice(ctx context.Context, s store.Tile) (io.Reader, func() error, error) {
	return f.DumpSliceOn(ctx, s, store.Tile{})
}

// DumpSliceOn is DumpSlice run in the container of instance tile on, a
// restored copy of the instance s lives on (the zero tile = s's own): the
// copy holds the same database under the same name.
func (f *Flow) DumpSliceOn(ctx context.Context, s, on store.Tile) (io.Reader, func() error, error) {
	p, ok, err := f.Instances.ProvisionOf(ctx, s.ID)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, errs.Conflictf("slice %s is not provisioned", s.Slug)
	}
	m, it, e, err := f.of(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	if on.ID != "" {
		it = on
		if m, err = f.Instances.GetByTile(ctx, on.ID); err != nil {
			return nil, nil, err
		}
	}
	d, ok := e.(sliceDumper)
	if !ok {
		return nil, nil, errs.Invalidf("engine", "%s slices cannot be branched; only postgres", m.Engine)
	}
	out, wait, err := f.Tiles.Stream(ctx, it, d.SliceDump(f.facts(ctx, it, m, e.Definition()), slice(p)), nil)
	if err != nil {
		return nil, nil, err
	}
	return out, func() error {
		err := wait()
		if x, ok := docker.IsExit(err); ok {
			return fmt.Errorf("dumping %s on %s failed: %s", p.DBName, it.Slug, stderrTail(x))
		}
		return err
	}, nil
}

// Seed loads dump (DumpCustom | DumpPlain) into slice tile s's database
// as the slice's owner, in one transaction: a failed load keeps nothing,
// and the error carries the tail of the tool's stderr. The slice must be
// provisioned (its deploy ran).
// The dump is any reader: an upload, or another slice's DumpSlice stream.
func (f *Flow) Seed(ctx context.Context, s store.Tile, format string, dump io.Reader, log io.Writer) error {
	p, ok, err := f.Instances.ProvisionOf(ctx, s.ID)
	if err != nil {
		return err
	}
	if !ok {
		return errs.Conflictf("slice %s is not provisioned", s.Slug)
	}
	m, it, e, err := f.of(ctx, p)
	if err != nil {
		return err
	}
	sd, ok := e.(seeder)
	if !ok {
		return errs.Invalidf("engine", "%s does not load dumps", m.Engine)
	}
	argv := sd.Seed(format, f.facts(ctx, it, m, e.Definition()), slice(p))
	if argv == nil {
		return errs.Invalidf("dump", "%q is not a dump format stackr loads", format)
	}
	logf(log, "loading the %s dump into %s on %s\n", format, p.DBName, it.Slug)
	out, wait, err := f.Tiles.Stream(ctx, it, argv, dump)
	if err == nil {
		_, _ = io.Copy(io.Discard, out)
		err = wait()
	}
	if x, ok := docker.IsExit(err); ok {
		return fmt.Errorf("loading the dump into %s failed, nothing was kept: %s", p.DBName, stderrTail(x))
	}
	if err != nil {
		return err
	}
	logf(log, "loaded the dump into %s\n", p.DBName)
	return nil
}

// stderrTail is the last lines of a failed command's stderr.
func stderrTail(x docker.ExitError) string {
	if x.Stderr == "" {
		return x.Error()
	}
	lines := strings.Split(strings.TrimSpace(x.Stderr), "\n")
	tail := strings.Join(lines[max(0, len(lines)-20):], "\n")
	if len(tail) > 2000 {
		tail = "…" + tail[len(tail)-2000:]
	}
	return tail
}
