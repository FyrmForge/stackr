// Package panel is stackrd's own container: find it, pull a new image, hand
// the swap to a one-shot helper, and the swap itself (run by that helper,
// since a process cannot outlive stopping its own container).
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// Docker is the slice of the daemon this leaf uses.
type Docker interface {
	Run(ctx context.Context, spec docker.ContainerSpec) (string, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	StopRemove(ctx context.Context, id string) error
	List(ctx context.Context, labels map[string]string) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (docker.Detail, error)
	Pull(ctx context.Context, ref, auth string, log io.Writer) error
	LocalDigest(ctx context.Context, ref string) (string, error)
}

const (
	LabelRole = "stackr.role"
	// SpecEnv carries the new panel's spec into the helper, as JSON.
	SpecEnv = installspec.SwapSpecEnv
)

type Leaf struct {
	docker Docker
	// Gate for the new panel: migrations may run before it answers.
	Poll, Grace, Deadline time.Duration
}

func New(d Docker) *Leaf {
	return &Leaf{
		docker:   d,
		Poll:     time.Second,
		Grace:    10 * time.Second,
		Deadline: 3 * time.Minute,
	}
}

// Pull fetches a panel image (public, anonymous). One already here is
// kept: a release tag never changes, and a box can be fed images by hand.
func (l *Leaf) Pull(ctx context.Context, image string, log io.Writer) error {
	if _, err := l.docker.LocalDigest(ctx, image); err == nil {
		return nil
	}
	return l.docker.Pull(ctx, image, "", log)
}

// Launch starts the helper that swaps the panel to spec: the new image
// running `stackrd upgrade-swap` with the socket. Refused while a helper
// still runs, which is the "already running" guard across panel restarts.
func (l *Leaf) Launch(ctx context.Context, spec docker.ContainerSpec) error {
	busy, err := l.docker.List(ctx, map[string]string{LabelRole: "upgrader"})
	if err != nil {
		return err
	}
	for _, c := range busy {
		if c.State == "running" {
			return errs.Conflictf("an upgrade is already running")
		}
		_ = l.docker.StopRemove(ctx, c.ID) // a finished helper from last time
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	vols := []string{"/var/run/docker.sock:/var/run/docker.sock"}
	if db := dbPath(spec); db != "" {
		// The swap copies the database aside, so the helper needs the data dir at the panel's path.
		dir := filepath.Dir(db)
		vols = append(vols, dir+":"+dir)
	}
	_, err = l.docker.Run(ctx, docker.ContainerSpec{
		// The image's entrypoint is /stackrd; Cmd is only its argument.
		Name:    "stackr-upgrader",
		Image:   spec.Image,
		Cmd:     []string{"upgrade-swap"},
		Env:     []string{SpecEnv + "=" + string(body)},
		Labels:  map[string]string{LabelRole: "upgrader"},
		Volumes: vols,
	})
	return err
}

// SpecFromEnv reads the helper's spec back.
func SpecFromEnv() (docker.ContainerSpec, error) {
	var s docker.ContainerSpec
	raw := os.Getenv(SpecEnv)
	if raw == "" {
		return s, errors.New(SpecEnv + " is not set")
	}
	return s, json.Unmarshal([]byte(raw), &s)
}

// dbPath is the database file the spec's panel opens ("" = unknown).
func dbPath(spec docker.ContainerSpec) string {
	for _, e := range spec.Env {
		if v, ok := strings.CutPrefix(e, "DATABASE_PATH="); ok {
			return v
		}
	}
	return ""
}

// The files that make one SQLite database; the copy keeps the suffixes.
var dbSuffixes = []string{"", "-wal", "-shm"}

// copyName is the per-upgrade copy of the database, named after the target.
func copyName(db string, spec docker.ContainerSpec) string {
	v := spec.Image[strings.LastIndex(spec.Image, ":")+1:]
	return db + ".pre-" + strings.ReplaceAll(v, "/", "_")
}

// Leftovers lists swap copies still beside the database: a helper that died
// mid-swap leaves one. Boot only reports them; they are never deleted there.
func Leftovers(db string) []string {
	m, _ := filepath.Glob(db + ".pre-*")
	return m
}

// saveDB copies the database files to the per-upgrade name, 0600 and synced.
func saveDB(db, cp string) error {
	for _, suf := range dbSuffixes {
		if err := copyFile(db+suf, cp+suf); failed(err, suf) {
			dropDB(cp)
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	return errors.Join(err, out.Close())
}

func dropDB(cp string) {
	for _, suf := range dbSuffixes {
		_ = os.Remove(cp + suf)
	}
}

// restoreDB puts the copy back over what the new panel migrated: the new
// panel's -wal/-shm go first, so none is replayed onto the old file.
func restoreDB(db, cp string) error {
	for _, suf := range dbSuffixes[1:] {
		_ = os.Remove(db + suf)
	}
	for _, suf := range dbSuffixes {
		if err := os.Rename(cp+suf, db+suf); failed(err, suf) {
			return err
		}
	}
	return nil
}

// Swap replaces the running panel with spec: stop the old one (kept), copy
// its database aside, run the new one, gate it like a tile; pass removes the
// old and the copy, fail removes the new, puts the database back and starts
// the old again. Plain containers have no Swarm to roll back a failed swap,
// so this is that rollback; the copy is needed because the new panel may
// have migrated the database past what the old one can open.
func (l *Leaf) Swap(ctx context.Context, spec docker.ContainerSpec) error {
	old, err := l.docker.List(ctx, map[string]string{LabelRole: "panel"})
	if err != nil {
		return err
	}
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels[LabelRole] = "panel"
	for _, c := range old {
		if err := l.docker.Stop(ctx, c.ID); err != nil {
			return fmt.Errorf("stop old panel: %w", err)
		}
	}
	back := func(cause error) error {
		for _, c := range old {
			if err := l.docker.Start(ctx, c.ID); err != nil {
				return errors.Join(cause, fmt.Errorf("start old panel: %w", err))
			}
		}
		return cause
	}
	db := dbPath(spec)
	cp := copyName(db, spec)
	if db != "" {
		if err := saveDB(db, cp); err != nil {
			return back(fmt.Errorf("copy database: %w", err))
		}
	}
	id, err := l.docker.Run(ctx, spec)
	if err != nil {
		if db != "" {
			dropDB(cp) // the new panel never ran: the database is untouched
		}
		return back(fmt.Errorf("run new panel: %w", err))
	}
	if err := docker.Healthy(ctx, l.docker.Inspect, id, l.Poll, l.Grace, l.Deadline); err != nil {
		_ = l.docker.StopRemove(ctx, id)
		if db != "" {
			if rerr := restoreDB(db, cp); rerr != nil {
				// Starting the old panel on a migrated database would only crash it.
				return errors.Join(fmt.Errorf("new panel: %w", err), fmt.Errorf("restore database from %s: %w", cp, rerr))
			}
		}
		return back(fmt.Errorf("new panel: %w", err))
	}
	if db != "" {
		dropDB(cp)
	}
	for _, c := range old {
		_ = l.docker.StopRemove(ctx, c.ID)
	}
	return nil
}

// failed reports a real error: a missing -wal or -shm is normal.
func failed(err error, suf string) bool {
	return err != nil && (suf == "" || !errors.Is(err, os.ErrNotExist))
}
