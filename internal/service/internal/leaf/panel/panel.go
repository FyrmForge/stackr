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
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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
	Tag(ctx context.Context, src, dst string) error
}

const (
	LabelRole = "stackr.role"
	// SpecEnv carries the new panel's spec into the helper, as JSON.
	SpecEnv = installspec.SwapSpecEnv
)

// RetryEnv marks a helper that Docker restarts when it dies (set by Launch).
const RetryEnv = "STACKR_SWAP_DOCKER_RETRY"

// ErrRolledBack wraps a swap error when the old panel is running again: the
// helper has nothing left to retry (it exits 0 on it).
var ErrRolledBack = errors.New("rolled back to the old panel")

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

// Tag points dst at the local image src: the digest a release declares,
// pulled, becomes the tag the rest of the upgrade runs by.
func (l *Leaf) Tag(ctx context.Context, src, dst string) error {
	return l.docker.Tag(ctx, src, dst)
}

// Digest is the registry digest the local image was pulled as ("" when it
// has none, a hand-fed or locally built one).
func (l *Leaf) Digest(ctx context.Context, image string) (string, error) {
	return l.docker.LocalDigest(ctx, image)
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
		Env:     []string{SpecEnv + "=" + string(body), RetryEnv + "=1"},
		Labels:  map[string]string{LabelRole: "upgrader"},
		Volumes: vols,
		// The gate dials the new panel's listen address, which is the host's.
		HostNetwork: true,
		// A helper that dies mid-swap (killed, or the daemon restarted) runs
		// again, and Swap starts over from whatever the first try left. A
		// swap that failed cleanly and rolled back also exits non-zero, so
		// the retries are bounded: three, then the old panel stays up.
		Restart: "on-failure:3",
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
	// A rerun (the helper died and Docker started it again) finds the new
	// panel of the first try, named spec.Name. It is not an old panel: it
	// goes, and the database copy of the first try goes back, so the swap
	// starts again from the panel that was running before.
	var stale []docker.Container
	old = slices.DeleteFunc(old, func(c docker.Container) bool {
		if c.Name == spec.Name {
			stale = append(stale, c)
		}
		return c.Name == spec.Name
	})
	// The first try died after its gate passed: the new panel is up and
	// healthy, and it has been writing. Finish the pass instead of undoing it.
	if len(stale) == 1 {
		if d, err := l.docker.Inspect(ctx, stale[0].ID); err == nil && d.Running && d.Health == "healthy" {
			if db := dbPath(spec); db != "" {
				dropDB(copyName(db, spec))
			}
			for _, c := range old {
				_ = l.docker.StopRemove(ctx, c.ID)
			}
			return nil
		}
	}
	for _, c := range old {
		if err := l.docker.Stop(ctx, c.ID); err != nil {
			return fmt.Errorf("stop old panel: %w", err)
		}
	}
	for _, c := range stale {
		if err := l.docker.StopRemove(ctx, c.ID); err != nil {
			return fmt.Errorf("remove the new panel of the first try: %w", err)
		}
	}
	back := func(cause error) error {
		for _, c := range old {
			if err := l.docker.Start(ctx, c.ID); err != nil {
				return errors.Join(cause, fmt.Errorf("start old panel: %w", err))
			}
		}
		return fmt.Errorf("%w: %w", ErrRolledBack, cause)
	}
	db := dbPath(spec)
	if _, err := os.Stat(db); db != "" && err != nil {
		// A helper launched by a panel built before the data-dir mount cannot see the file.
		slog.Warn("upgrade: database not visible to the helper, swapping without a copy", "file", db, "err", err)
		db = ""
	}
	cp := copyName(db, spec)
	if db != "" {
		if _, err := os.Stat(cp); err == nil { // left by the first try
			if err := restoreDB(db, cp); err != nil {
				return back(fmt.Errorf("restore the database of the first try: %w", err))
			}
		}
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
	err = docker.Healthy(ctx, l.docker.Inspect, id, l.Poll, l.Grace, l.Deadline)
	if err == nil {
		err = l.probe(ctx, spec)
	}
	if err != nil {
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

// probe asks the new panel's /api/health over HTTP until it answers 200 or
// the deadline passes: the panel has no HEALTHCHECK, so the container gate
// alone passes a panel that runs but serves nothing. The address is the
// spec's HOST and PORT; a spec with no PORT (a test's) is not probed.
func (l *Leaf) probe(ctx context.Context, spec docker.ContainerSpec) error {
	var host, port string
	for _, e := range spec.Env {
		if v, ok := strings.CutPrefix(e, "HOST="); ok {
			host = v
		}
		if v, ok := strings.CutPrefix(e, "PORT="); ok {
			port = v
		}
	}
	if port == "" {
		return nil
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/api/health"
	ctx, cancel := context.WithTimeout(ctx, l.Deadline)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the panel did not answer %s with 200 before the deadline", url)
		case <-time.After(l.Poll):
		}
	}
}

// failed reports a real error: a missing -wal or -shm is normal.
func failed(err error, suf string) bool {
	return err != nil && (suf == "" || !errors.Is(err, os.ErrNotExist))
}
