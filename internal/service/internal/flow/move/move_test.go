package move

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

type nodes struct{ src, dst *dockerfake.Fake }

func (n nodes) Docker(id string) (Docker, error) {
	switch id {
	case "a":
		return n.src, nil
	case "b":
		return n.dst, nil
	}
	return nil, errors.New("no such node " + id)
}

func (n nodes) Addr(_ context.Context, id string) (string, error) {
	return map[string]string{"a": "10.1.0.1", "b": "10.1.0.2"}[id], nil
}

// env records what the flow asked of the env, in order.
type env struct {
	mu   sync.Mutex
	log  []string
	vols []store.Volume
	err  map[string]error
	// guard runs inside Stop: the copy must not have begun.
	guard func()
}

func (e *env) rec(s string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
	return e.err[s]
}

func (e *env) Volumes(context.Context, string) ([]store.Volume, error) { return e.vols, nil }
func (e *env) Stop(_ context.Context, _ string) error {
	if e.guard != nil {
		e.guard()
	}
	return e.rec("stop")
}
func (e *env) Start(_ context.Context, _, node string) error  { return e.rec("start " + node) }
func (e *env) Remove(_ context.Context, _, node string) error { return e.rec("remove " + node) }

type rig struct {
	f        *Flow
	src, dst *dockerfake.Fake
	env      *env
	moves    store.MoveStore
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st := storetest.Store(t)
	now := time.Now()
	for _, q := range []string{
		`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,'',?,'','','','',0,NULL)`,
		`INSERT INTO stacks VALUES ('s1','o1','Shop','shop','','{}','','','','',?,NULL)`,
		`INSERT INTO environments VALUES ('e1','s1','Dev','dev','static',NULL,'{}','',0,'net',NULL,'branch','main',1,?,1,'','')`,
	} {
		if _, err := st.DB().Exec(q, now); err != nil {
			t.Fatal(err)
		}
	}
	src, dst := dockerfake.New(), dockerfake.New()
	src.RunID = "helper"
	dst.RunID = "helper"
	src.TarOut = []byte("tar bytes")
	e := &env{vols: []store.Volume{{ID: "v1", Slug: "data", Name: "stackr-vol-v1"}}}
	m := store.NewMoves(st.DB(), nil)
	return &rig{&Flow{Moves: m, Nodes: nodes{src, dst}, Env: e}, src, dst, e, m}
}

func (r *rig) begin(t *testing.T, live bool) store.EnvMove {
	t.Helper()
	m, err := r.f.Begin(context.Background(), Request{EnvID: "e1", From: "a", To: "b", Live: live})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func methods(f *dockerfake.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		out = append(out, c.Method)
	}
	return out
}

func TestColdMove(t *testing.T) {
	r := newRig(t)
	r.env.guard = func() {
		if slices.Contains(methods(r.src), "TarVolume") {
			t.Error("copied before the env was stopped")
		}
	}
	m := r.begin(t, false)
	got, err := r.f.Run(context.Background(), m.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != Done || got.FinishedAt == nil || got.Bytes != int64(len("tar bytes")) {
		t.Fatalf("move = %+v", got)
	}
	if want := []string{"stop", "start b", "remove a"}; !slices.Equal(r.env.log, want) {
		t.Fatalf("env calls = %v, want %v", r.env.log, want)
	}
	if !bytes.Equal(r.dst.Untarred, r.src.TarOut) {
		t.Fatalf("target got %q", r.dst.Untarred)
	}
	if len(r.dst.Created) != 1 || r.dst.Created[0].Name != "stackr-vol-v1" {
		t.Fatalf("target volumes = %+v", r.dst.Created)
	}
	if slices.Contains(methods(r.src), "RemoveVolume") {
		t.Fatal("old volume must be kept")
	}
	saved, _ := r.moves.Get(context.Background(), m.ID)
	if saved.Phase != Done {
		t.Fatalf("saved phase %s", saved.Phase)
	}
}

func TestColdCopyFailureRollsBack(t *testing.T) {
	r := newRig(t)
	r.src.Err = map[string]error{"TarVolume": errors.New("disk gone")}
	m := r.begin(t, false)
	got, err := r.f.Run(context.Background(), m.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "disk gone") {
		t.Fatalf("err = %v", err)
	}
	if got.Phase != RolledBack || got.Error == "" {
		t.Fatalf("move = %+v", got)
	}
	if want := []string{"stop", "start a"}; !slices.Equal(r.env.log, want) {
		t.Fatalf("env calls = %v, want %v", r.env.log, want)
	}
}

func TestPrepareFailureLeavesEnvAlone(t *testing.T) {
	r := newRig(t)
	r.dst.Err = map[string]error{"EnsureTool": errors.New("target down")}
	m := r.begin(t, false)
	got, err := r.f.Run(context.Background(), m.ID, nil)
	if err == nil || got.Phase != RolledBack {
		t.Fatalf("move = %+v err = %v", got, err)
	}
	if len(r.env.log) != 0 {
		t.Fatalf("env touched: %v", r.env.log)
	}
}

func TestResumeFromRecordedPhase(t *testing.T) {
	r := newRig(t)
	m := r.begin(t, false)
	m.Phase = Copying // crashed after the stop
	if err := r.moves.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	un, _ := r.f.Unfinished(context.Background())
	if len(un) != 1 {
		t.Fatalf("unfinished = %v", un)
	}
	got, err := r.f.Run(context.Background(), m.ID, nil)
	if err != nil || got.Phase != Done {
		t.Fatalf("move = %+v err = %v", got, err)
	}
	if want := []string{"start b", "remove a"}; !slices.Equal(r.env.log, want) {
		t.Fatalf("env calls = %v, want %v", r.env.log, want)
	}
	if un, _ = r.f.Unfinished(context.Background()); len(un) != 0 {
		t.Fatalf("unfinished after = %v", un)
	}
}

func TestStartFailureRemovesTargetAndRestartsSource(t *testing.T) {
	r := newRig(t)
	r.env.err = map[string]error{"start b": errors.New("no room")}
	m := r.begin(t, false)
	got, _ := r.f.Run(context.Background(), m.ID, nil)
	if got.Phase != RolledBack {
		t.Fatalf("move = %+v", got)
	}
	if want := []string{"stop", "start b", "remove b", "start a"}; !slices.Equal(r.env.log, want) {
		t.Fatalf("env calls = %v, want %v", r.env.log, want)
	}
}

func TestCleaningFailureIsRetried(t *testing.T) {
	r := newRig(t)
	r.env.err = map[string]error{"remove a": errors.New("busy")}
	m := r.begin(t, false)
	got, err := r.f.Run(context.Background(), m.ID, nil)
	if err == nil || got.Phase != Cleaning || got.Error == "" {
		t.Fatalf("move = %+v err = %v", got, err)
	}
	r.env.err = nil
	if got, err = r.f.Run(context.Background(), m.ID, nil); err != nil || got.Phase != Done {
		t.Fatalf("retry = %+v err = %v", got, err)
	}
	// A move already on the target cannot be rolled back.
	if _, err := r.f.Rollback(context.Background(), got, nil, nil); err == nil {
		t.Fatal("rollback of a finished move succeeded")
	}
}

func TestOneUnfinishedMovePerEnv(t *testing.T) {
	r := newRig(t)
	r.begin(t, false)
	if _, err := r.f.Begin(context.Background(), Request{EnvID: "e1", From: "a", To: "b"}); err == nil {
		t.Fatal("second move accepted")
	}
	if _, err := r.f.Begin(context.Background(), Request{EnvID: "e1", From: "a", To: "a"}); err == nil {
		t.Fatal("move to the same node accepted")
	}
}

func TestLiveMove(t *testing.T) {
	r := newRig(t)
	m := r.begin(t, true)
	got, err := r.f.Run(context.Background(), m.ID, nil)
	if err != nil || got.Phase != Done {
		t.Fatalf("move = %+v err = %v", got, err)
	}
	if want := []string{"stop", "start b", "remove a"}; !slices.Equal(r.env.log, want) {
		t.Fatalf("env calls = %v, want %v", r.env.log, want)
	}
	// Two passes: a daemon on the target and a client on the source each.
	if len(r.dst.Specs) != 2 || len(r.src.Specs) != 2 {
		t.Fatalf("helpers: dst %d src %d", len(r.dst.Specs), len(r.src.Specs))
	}
	d := r.dst.Specs[0]
	if d.Image != docker.ToolImage || d.Env[0] != "RSYNC_ALLOW=10.1.0.1" || d.Ports["8873"] != "873" ||
		!strings.Contains(d.Cmd[2], "rsync --daemon") {
		t.Fatalf("daemon spec = %+v", d)
	}
	first, second := r.src.Specs[0], r.src.Specs[1]
	if first.Volumes[0] != "stackr-vol-v1:/data:ro" || !strings.Contains(first.Cmd[2], "rsync://10.1.0.2:8873/data/") {
		t.Fatalf("client spec = %+v", first)
	}
	if strings.Contains(first.Cmd[2], "--checksum") || !strings.Contains(second.Cmd[2], "--checksum") {
		t.Fatalf("checksum only on the final pass: %q / %q", first.Cmd[2], second.Cmd[2])
	}
	if slices.Contains(methods(r.dst), "UntarVolume") {
		t.Fatal("live move must not tar")
	}
	// Every helper is removed again.
	removed := 0
	for _, c := range r.dst.Calls() {
		if c.Method == "StopRemove" && c.Args[0] == "helper" {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("daemons removed = %d", removed)
	}
}

func TestLiveSyncFailureNeverStopsTheEnv(t *testing.T) {
	r := newRig(t)
	r.src.ExitCode = 23 // rsync: partial transfer
	m := r.begin(t, true)
	got, err := r.f.Run(context.Background(), m.ID, nil)
	if err == nil || got.Phase != RolledBack {
		t.Fatalf("move = %+v err = %v", got, err)
	}
	if len(r.env.log) != 0 {
		t.Fatalf("env touched: %v", r.env.log)
	}
}

func TestLiveFinalFailureRestartsSource(t *testing.T) {
	r := newRig(t)
	m := r.begin(t, true)
	m.Phase = Final
	if err := r.moves.Update(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	r.src.ExitCode = 1
	got, _ := r.f.Run(context.Background(), m.ID, nil)
	if got.Phase != RolledBack {
		t.Fatalf("move = %+v", got)
	}
	if want := []string{"start a"}; !slices.Equal(r.env.log, want) {
		t.Fatalf("env calls = %v, want %v", r.env.log, want)
	}
}

func TestCancelledRunResumesInsteadOfRollingBack(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.env.guard = cancel
	r.env.err = map[string]error{"stop": context.Canceled}
	m := r.begin(t, false)
	if _, err := r.f.Run(ctx, m.ID, nil); err == nil {
		t.Fatal("cancelled run reported success")
	}
	saved, _ := r.moves.Get(context.Background(), m.ID)
	if saved.Phase != Stopping {
		t.Fatalf("phase = %s, want stopping", saved.Phase)
	}
}
