package panel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/hostdisk/hostdisktest"
	"github.com/FyrmForge/stackr/internal/paneldb"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// moveDocker is a small stateful daemon: Run adds a container, Stop and
// StopRemove change it, List shows what is there.
type moveDocker struct {
	mu      sync.Mutex
	cs      []docker.Container
	health  map[string]string // container name -> Health the gate sees
	n       int
	runFail bool
	calls   []string
}

func (d *moveDocker) add(name string, running bool) string {
	d.n++
	id := fmt.Sprintf("c%d", d.n)
	st := "exited"
	if running {
		st = "running"
	}
	d.cs = append(d.cs, docker.Container{ID: id, Name: name, State: st, Labels: map[string]string{LabelRole: "panel"}})
	return id
}

func (d *moveDocker) Run(_ context.Context, s docker.ContainerSpec) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "Run "+s.Name)
	if d.runFail {
		return "", errors.New("docker: cannot start")
	}
	return d.add(s.Name, true), nil
}
func (d *moveDocker) set(id, state string) {
	for i := range d.cs {
		if d.cs[i].ID == id {
			d.cs[i].State = state
		}
	}
}
func (d *moveDocker) Start(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "Start "+id)
	d.set(id, "running")
	return nil
}
func (d *moveDocker) Stop(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "Stop "+id)
	d.set(id, "exited")
	return nil
}
func (d *moveDocker) StopRemove(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "StopRemove "+id)
	d.cs = slices.DeleteFunc(d.cs, func(c docker.Container) bool { return c.ID == id })
	return nil
}
func (d *moveDocker) List(_ context.Context, labels map[string]string) ([]docker.Container, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []docker.Container
	for _, c := range d.cs {
		if matches(c.Labels, labels) {
			out = append(out, c)
		}
	}
	return out, nil
}
func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
func (d *moveDocker) Inspect(_ context.Context, id string) (docker.Detail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.cs {
		if c.ID == id {
			det := docker.Detail{Running: c.State == "running"}
			if det.Running {
				det.Health = "healthy"
				if h, ok := d.health[c.Name]; ok {
					det.Health = h
				}
			}
			return det, nil
		}
	}
	return docker.Detail{}, errors.New("no such container")
}
func (d *moveDocker) Logs(context.Context, string, int) (string, error)     { return "log", nil }
func (d *moveDocker) Pull(context.Context, string, string, io.Writer) error { return nil }
func (d *moveDocker) LocalDigest(context.Context, string) (string, error)   { return "", nil }
func (d *moveDocker) Tag(context.Context, string, string) error             { return nil }
func (d *moveDocker) Wait(context.Context, string) (int, error)             { return 0, nil }

// hostDisk runs requests on the fake host, the way the privileged helper does,
// with the host's data dir path translated to the temp root.
type hostDisk struct {
	h *hostdisktest.Host
	n int
}

func (d *hostDisk) Disk(ctx context.Context, r hostdisk.Request) (hostdisk.Result, error) {
	d.n++
	if r.DB != nil {
		q := *r.DB
		q.DataDir = strings.TrimPrefix(q.DataDir, d.h.Root)
		r.DB = &q
	}
	return d.h.Run(ctx, r), nil
}

// env is one test machine: the host, the daemon, a database with a row that
// only the -wal holds, and the old panel running.
type env struct {
	t       *testing.T
	h       *hostdisktest.Host
	d       *moveDocker
	dd      *hostDisk
	dataDir string
	rows    []string
	db      *sql.DB // kept open: the newest row lives in the -wal
}

func newEnv(t *testing.T) *env {
	h := hostdisktest.New(t, "d1")
	e := &env{t: t, h: h, d: &moveDocker{health: map[string]string{}}, dd: &hostDisk{h: h}, dataDir: h.Root + h.DataDir}
	if err := os.WriteFile(filepath.Join(e.dataDir, paneldb.InstallFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := paneldb.EnsureLayout(e.dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(e.dataDir, paneldb.DirName), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+h.DB()+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TABLE t(v TEXT); INSERT INTO t VALUES ('old'); INSERT INTO t VALUES ('wal-only')"); err != nil {
		t.Fatal(err)
	}
	e.db = db
	e.rows = []string{"old", "wal-only"}
	t.Cleanup(func() { _ = db.Close() })
	e.d.add("stackr-old", true)
	return e
}

func (e *env) spec(name string) docker.ContainerSpec {
	return docker.ContainerSpec{Name: name, Image: "img:1", Env: []string{"DATA_DIR=" + e.dataDir, "DATABASE_PATH=" + paneldb.Path(e.dataDir)}}
}

func (e *env) leaf(crash func(string) error) *Leaf {
	l := New(e.d)
	l.Poll, l.Grace, l.Deadline = time.Millisecond, time.Millisecond, 20*time.Millisecond
	l.Disk = e.dd
	l.crashAt = crash
	return l
}

func mvIn() Move {
	return Move{Dir: paneldb.DirIn, MoveID: "m1abcdef", DiskID: "d1", Folder: "f", InstallID: "inst", Stamp: "20261126T100000Z"}
}

func mvBack() Move {
	m := mvIn()
	m.Dir, m.MoveID, m.Stamp = paneldb.DirBack, "m2abcdef", "20261126T110000Z"
	return m
}

func (e *env) journal() paneldb.Journal {
	j, err := paneldb.ReadJournal(e.dataDir)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func readRows(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rs, err := db.Query("SELECT v FROM t ORDER BY rowid")
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer func() { _ = rs.Close() }()
	var out []string
	for rs.Next() {
		var v string
		if err := rs.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func (e *env) running(name string) bool {
	cs, _ := e.d.List(context.Background(), map[string]string{LabelRole: "panel"})
	for _, c := range cs {
		if c.Name == name && c.State == "running" {
			return true
		}
	}
	return false
}

func (e *env) swapNames() []string {
	ents, _ := os.ReadDir(paneldb.Swap(e.dataDir))
	var out []string
	for _, en := range ents {
		out = append(out, en.Name())
	}
	return out
}

// assertMoved: the database is on the disk, nothing is half done.
func (e *env) assertMoved(newName string) {
	e.t.Helper()
	j := e.journal()
	if j.Phase != paneldb.PhaseMoved {
		e.t.Fatalf("journal = %+v, want moved", j)
	}
	if !e.h.Mounted() || !e.h.Locked() {
		e.t.Errorf("mounted=%v underlay locked=%v", e.h.Mounted(), e.h.Locked())
	}
	if got := readRows(e.t, e.h.DB()); !slices.Equal(got, e.rows) {
		e.t.Errorf("rows through the mount = %v", got)
	}
	if !e.running(newName) || e.running("stackr-old") {
		e.t.Errorf("containers: new running=%v old running=%v", e.running(newName), e.running("stackr-old"))
	}
	var copies int
	for _, n := range e.swapNames() {
		switch {
		case strings.HasPrefix(n, paneldb.FileName+".moved-"):
			copies++
			if strings.HasSuffix(n, "-wal") {
				copies--
			}
			if !strings.HasSuffix(n, "-wal") {
				if got := readRows(e.t, filepath.Join(paneldb.Swap(e.dataDir), n)); !slices.Equal(got, e.rows) {
					e.t.Errorf("old copy rows = %v", got)
				}
			}
		case n == movingName || n == movingName+"-wal" || n == incomingName:
			e.t.Errorf("leftover %s", n)
		}
	}
	if copies != 1 {
		e.t.Errorf("old copies = %d in %v", copies, e.swapNames())
	}
}

// assertUnmoved: the database is on the system disk, the old panel runs, and
// the disk side holds no live database.
func (e *env) assertUnmoved(oldName string) {
	e.t.Helper()
	j := e.journal()
	if j.Phase != paneldb.PhaseNone {
		e.t.Fatalf("journal = %+v, want none", j)
	}
	if e.h.Mounted() || e.h.Locked() {
		e.t.Errorf("mounted=%v locked=%v", e.h.Mounted(), e.h.Locked())
	}
	if got := readRows(e.t, e.h.DB()); !slices.Equal(got, e.rows) {
		e.t.Errorf("rows on the system disk = %v", got)
	}
	if !e.running(oldName) {
		e.t.Errorf("old panel %s is not running: %v", oldName, e.d.cs)
	}
	for _, n := range e.swapNames() {
		if n == movingName || n == movingName+"-wal" || n == incomingName {
			e.t.Errorf("leftover %s", n)
		}
	}
}

func TestMoveInAndBackHappyPath(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.leaf(nil).Move(ctx, e.spec("stackr-new"), mvIn()); err != nil {
		t.Fatal(err)
	}
	e.assertMoved("stackr-new")
	// the folder has the marker and the database
	if _, err := os.Stat(filepath.Join(e.h.Folder(), hostdisk.PanelDBMarker)); err != nil {
		t.Error(err)
	}
	// a repeat of the same helper run is a no-op
	if err := e.leaf(nil).Move(ctx, e.spec("stackr-new"), mvIn()); err != nil {
		t.Errorf("retry after the pass: %v", err)
	}
	// and back
	if err := e.leaf(nil).Move(ctx, e.spec("stackr-back"), mvBack()); err != nil {
		t.Fatal(err)
	}
	j := e.journal()
	if j.Phase != paneldb.PhaseNone || e.h.Mounted() || e.h.Locked() {
		t.Fatalf("after back: journal %+v mounted=%v locked=%v", j, e.h.Mounted(), e.h.Locked())
	}
	if got := readRows(t, e.h.DB()); !slices.Equal(got, e.rows) {
		t.Errorf("rows after back = %v", got)
	}
	if !e.running("stackr-back") || e.running("stackr-new") {
		t.Errorf("containers %v", e.d.cs)
	}
	ents, _ := os.ReadDir(e.h.Folder())
	var names []string
	for _, en := range ents {
		names = append(names, en.Name())
	}
	if len(names) != 1 || !strings.HasPrefix(names[0], paneldb.FileName+".moved-") {
		t.Errorf("disk folder after back = %v (marker gone, copy renamed)", names)
	}
}

// The newest row exists only in the -wal; the copy must hold it.
func TestWalOnlyRowEndsInTheCopy(t *testing.T) {
	e := newEnv(t)
	if _, err := os.Stat(e.h.DB() + "-wal"); err != nil {
		t.Fatalf("no -wal to test with: %v", err)
	}
	if err := e.leaf(nil).Move(context.Background(), e.spec("stackr-new"), mvIn()); err != nil {
		t.Fatal(err)
	}
	only := readRows(t, filepath.Join(e.h.Folder(), paneldb.FileName))
	if !slices.Contains(only, "wal-only") {
		t.Errorf("the disk copy rows = %v", only)
	}
}

func TestVerifyFailureRollsBackWithTheSourceUntouched(t *testing.T) {
	e := newEnv(t)
	e.h.FailOn = func(string) error { return nil }
	// the integrity check of the copy fails
	l := e.leaf(nil)
	bad := *e.h
	_ = bad
	e.h.Env.Integrity = func(context.Context, string) error { return errors.New("malformed") }
	err := l.Move(context.Background(), e.spec("stackr-new"), mvIn())
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("err = %v", err)
	}
	e.assertUnmoved("stackr-old")
}

func TestGateFailureRollsBack(t *testing.T) {
	e := newEnv(t)
	e.d.health["stackr-new"] = "unhealthy"
	err := e.leaf(nil).Move(context.Background(), e.spec("stackr-new"), mvIn())
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("err = %v", err)
	}
	e.assertUnmoved("stackr-old")
	if e.running("stackr-new") {
		t.Error("the failed new panel still runs")
	}
	if _, err := os.Stat(FailureFile(paneldb.Path(e.dataDir))); err != nil {
		t.Errorf("no failure file in swap/: %v", err)
	}
}

func TestDiskFullDuringTheCopyRollsBack(t *testing.T) {
	e := newEnv(t)
	e.h.FailOn = nil
	// the copy op fails as a full disk would
	full := &failingDisk{inner: e.dd, op: "copy"}
	l := e.leaf(nil)
	l.Disk = full
	err := l.Move(context.Background(), e.spec("stackr-new"), mvIn())
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("err = %v", err)
	}
	e.assertUnmoved("stackr-old")
}

type failingDisk struct {
	inner DiskRunner
	op    string
}

func (f *failingDisk) Disk(ctx context.Context, r hostdisk.Request) (hostdisk.Result, error) {
	if r.Op == f.op {
		return hostdisk.Result{Error: &hostdisk.Error{Code: hostdisk.CodeIO, Message: "no space left on device"}}, nil
	}
	return f.inner.Disk(ctx, r)
}

// recording: which requests ran
type recDisk struct {
	inner DiskRunner
	reqs  []hostdisk.Request
}

func (r *recDisk) Disk(ctx context.Context, q hostdisk.Request) (hostdisk.Result, error) {
	r.reqs = append(r.reqs, q)
	return r.inner.Disk(ctx, q)
}

// Every request that names db/ says whether it must be mounted: the helper
// never takes it for granted.
func TestEveryDBRequestSaysTheMountState(t *testing.T) {
	e := newEnv(t)
	rec := &recDisk{inner: e.dd}
	l := e.leaf(nil)
	l.Disk = rec
	if err := l.Move(context.Background(), e.spec("stackr-new"), mvIn()); err != nil {
		t.Fatal(err)
	}
	for _, r := range rec.reqs {
		if r.DB == nil {
			t.Fatalf("a request without a database part: %+v", r)
		}
		touches := r.DB.From.Area == hostdisk.AreaDB || r.DB.To.Area == hostdisk.AreaDB || r.Op == "lock" || r.Op == "unlock" || r.Op == "clear-underlay"
		if touches && r.DB.Mounted != "yes" && r.DB.Mounted != "no" {
			t.Errorf("%s names db/ without a mount state: %+v", r.Op, r.DB)
		}
	}
}

// The helper process never opens db/ through its own bind: the move code has no
// file call of its own, only the journal and the disk helper.
func TestMoveCodeHasNoLocalFileAccess(t *testing.T) {
	src, err := os.ReadFile("move.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"os.Open", "os.ReadFile", "os.Stat", "os.Rename", "os.Remove", "os.Create", "os.WriteFile", "os.OpenFile", "os.ReadDir", "os.Lstat"} {
		if strings.Contains(string(src), bad+"(") {
			t.Errorf("move.go calls %s: every db/ file operation goes through the disk helper", bad)
		}
	}
}

// crashPoints runs a move once and lists the steps it passes, the rollback's
// too when the new panel is unhealthy.
func crashPoints(t *testing.T, dir, gate string) []string {
	e := newEnv(t)
	var steps []string
	rec := func(s string) error { steps = append(steps, s); return nil }
	ctx := context.Background()
	settle := func(err error) {
		if err != nil && !(gate == "unhealthy" && errors.Is(err, ErrRolledBack)) {
			t.Fatal(err)
		}
	}
	if dir == paneldb.DirBack {
		if err := e.leaf(nil).Move(ctx, e.spec("stackr-new"), mvIn()); err != nil {
			t.Fatal(err)
		}
		e.d.health["stackr-back"] = gate
		steps = nil
		settle(e.leaf(rec).Move(ctx, e.spec("stackr-back"), mvBack()))
		return steps
	}
	e.d.health["stackr-new"] = gate
	settle(e.leaf(rec).Move(ctx, e.spec("stackr-new"), mvIn()))
	return steps
}

// A crash at every step of the move in, with the gate passing or failing: the
// next run of the helper (or the installer) ends with the database on exactly
// one side, whole, and the other side clean.
func TestMoveInCrashAtEveryStep(t *testing.T) {
	for _, gate := range []string{"healthy", "unhealthy"} {
		steps := crashPoints(t, paneldb.DirIn, gate)
		if len(steps) < 8 || gate == "unhealthy" && !slices.Contains(steps, "rb-renamed") {
			t.Fatalf("only %d steps: %v", len(steps), steps)
		}
		for i := range steps {
			t.Run(fmt.Sprintf("%s/%d-%s", gate, i, steps[i]), func(t *testing.T) {
				e := newEnv(t)
				e.d.health["stackr-new"] = gate
				ctx := context.Background()
				n := -1
				crash := func(string) error {
					if n++; n == i {
						return errCrash
					}
					return nil
				}
				err := e.leaf(crash).Move(ctx, e.spec("stackr-new"), mvIn())
				if gate == "healthy" && !errors.Is(err, errCrash) && err != nil {
					t.Fatalf("first run: %v", err)
				}
				finish(t, e, ctx, e.spec("stackr-new"), mvIn(), gate == "healthy", "stackr-old")
			})
		}
	}
}

// finish runs the helper again until it settles (a crash inside a rollback
// resumes the same rollback) and checks the end state.
func finish(t *testing.T, e *env, ctx context.Context, spec docker.ContainerSpec, mv Move, wantMoved bool, oldName string) {
	t.Helper()
	var err error
	for try := 0; try < 3; try++ {
		err = e.leaf(nil).Move(ctx, spec, mv)
		if err == nil || errors.Is(err, ErrRolledBack) {
			break
		}
	}
	j := e.journal()
	switch {
	case err == nil && j.Phase == paneldb.PhaseMoved && mv.Dir == paneldb.DirIn:
		if !wantMoved {
			t.Fatalf("an unhealthy new panel ended moved")
		}
		e.assertMoved(spec.Name)
	case err == nil && j.Phase == paneldb.PhaseNone && mv.Dir == paneldb.DirBack:
		if got := readRows(t, e.h.DB()); !slices.Equal(got, e.rows) || e.h.Mounted() || e.h.Locked() {
			t.Fatalf("back: rows %v mounted=%v locked=%v", got, e.h.Mounted(), e.h.Locked())
		}
	case errors.Is(err, ErrRolledBack):
		if mv.Dir == paneldb.DirIn {
			e.assertUnmoved(oldName)
		} else {
			if j.Phase != paneldb.PhaseMoved || !e.h.Mounted() {
				t.Fatalf("back rolled back: journal %+v mounted=%v", j, e.h.Mounted())
			}
			if got := readRows(t, e.h.DB()); !slices.Equal(got, e.rows) {
				t.Fatalf("rows on the disk after a rolled back move-back = %v", got)
			}
			if !e.running(oldName) {
				t.Fatalf("old panel not running: %v", e.d.cs)
			}
		}
	default:
		t.Fatalf("did not settle: err=%v journal=%+v", err, j)
	}
}

func TestMoveBackCrashAtEveryStep(t *testing.T) {
	for _, gate := range []string{"healthy", "unhealthy"} {
		steps := crashPoints(t, paneldb.DirBack, gate)
		if len(steps) < 6 || gate == "unhealthy" && !slices.Contains(steps, "rolled-back-files") {
			t.Fatalf("only %d steps: %v", len(steps), steps)
		}
		for i := range steps {
			t.Run(fmt.Sprintf("%s/%d-%s", gate, i, steps[i]), func(t *testing.T) {
				e := newEnv(t)
				ctx := context.Background()
				if err := e.leaf(nil).Move(ctx, e.spec("stackr-new"), mvIn()); err != nil {
					t.Fatal(err)
				}
				e.d.health["stackr-back"] = gate
				n := -1
				crash := func(string) error {
					if n++; n == i {
						return errCrash
					}
					return nil
				}
				err := e.leaf(crash).Move(ctx, e.spec("stackr-back"), mvBack())
				if gate == "healthy" && err != nil && !errors.Is(err, errCrash) {
					t.Fatalf("first run: %v", err)
				}
				finish(t, e, ctx, e.spec("stackr-back"), mvBack(), gate == "healthy", "stackr-new")
			})
		}
	}
}

// A host command that fails at any point: the move rolls back, or resumes, and
// still ends clean on one side.
func TestMoveInFailsAtEveryHostCommand(t *testing.T) {
	probe := newEnv(t)
	if err := probe.leaf(nil).Move(context.Background(), probe.spec("stackr-new"), mvIn()); err != nil {
		t.Fatal(err)
	}
	total := len(probe.h.Commands)
	if total < 5 {
		t.Fatalf("only %d host commands", total)
	}
	for i := 0; i < total; i++ {
		t.Run(fmt.Sprintf("cmd-%d", i), func(t *testing.T) {
			e := newEnv(t)
			n := -1
			e.h.FailOn = func(string) error {
				if n++; n == i {
					return errors.New("injected")
				}
				return nil
			}
			ctx := context.Background()
			err := e.leaf(nil).Move(ctx, e.spec("stackr-new"), mvIn())
			e.h.FailOn = nil
			switch {
			case err == nil:
				e.assertMoved("stackr-new")
			default:
				// rolled back, or the rollback itself failed and left the journal rolling-back
				finish(t, e, ctx, e.spec("stackr-new"), mvIn(), true, "stackr-old")
			}
		})
	}
}

func TestSecondMoveWhileOneRunsIsRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	crash := func(s string) error {
		if s == "stopped" {
			return errCrash
		}
		return nil
	}
	_ = e.leaf(crash).Move(ctx, e.spec("stackr-new"), mvIn())
	other := mvIn()
	other.MoveID = "other-id"
	if err := e.leaf(nil).Move(ctx, e.spec("stackr-new"), other); err == nil || !strings.Contains(err.Error(), "another move") {
		t.Errorf("a different move over a journal in progress: %v", err)
	}
}
