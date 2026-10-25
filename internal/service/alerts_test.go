package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

type hookHit struct {
	a   Alert
	sig string
	raw []byte
}

// hook is a webhook receiver; codes answers the first posts in order, 200 after.
type hook struct {
	srv   *httptest.Server
	got   chan hookHit
	mu    sync.Mutex
	codes []int
}

func newHook(t *testing.T, codes ...int) *hook {
	t.Helper()
	h := &hook{got: make(chan hookHit, 32), codes: codes}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var a Alert
		_ = json.Unmarshal(b, &a)
		h.got <- hookHit{a, r.Header.Get(AlertSignatureHeader), b}
		h.mu.Lock()
		code := http.StatusOK
		if len(h.codes) > 0 {
			code, h.codes = h.codes[0], h.codes[1:]
		}
		h.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hook) next(t *testing.T) hookHit {
	t.Helper()
	select {
	case x := <-h.got:
		return x
	case <-time.After(5 * time.Second):
		t.Fatal("no alert arrived")
	}
	return hookHit{}
}

// events are the alerts that arrive before a sentinel queued now: the queue
// is FIFO, so the sentinel proves nothing else is on the way.
func (h *hook) events(t *testing.T, o *Orchestrator) []string {
	t.Helper()
	o.alert(Alert{Event: "sentinel"})
	var out []string
	for {
		x := h.next(t)
		if x.a.Event == "sentinel" {
			return out
		}
		out = append(out, x.a.Event+":"+x.a.Severity)
	}
}

func alertsTo(t *testing.T, o *Orchestrator, h *hook, secret string) {
	t.Helper()
	s := secret
	must(t, o.SetAlerts(context.Background(), h.srv.URL+"/hook/token", &s))
}

// The test send arrives signed with the stored secret; the secret is sealed
// at rest, the URL masked and the delivery recorded.
func TestAlertTestSendIsSigned(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	if err := o.TestAlert(ctx); err != errAlertsOff {
		t.Fatalf("test with no URL = %v", err)
	}
	h := newHook(t)
	alertsTo(t, o, h, "s3cret")
	must(t, o.TestAlert(ctx))
	x := h.next(t)
	if x.a.Event != AlertTest || x.sig != alertSig("s3cret", x.raw) || !strings.HasPrefix(x.sig, "sha256=") {
		t.Errorf("test alert = %+v sig %q", x.a, x.sig)
	}
	if _, err := time.Parse(time.RFC3339, x.a.At); err != nil {
		t.Errorf("at = %q", x.a.At)
	}
	raw, _, _ := o.store.Settings.Get(ctx, alertSecretKey)
	if !strings.HasPrefix(raw, "enc1:") {
		t.Errorf("secret stored as %q", raw)
	}
	s, err := o.AlertSettings(ctx)
	must(t, err)
	if s.URL != h.srv.URL+"/…" || !s.SecretSet || s.Last == nil || !s.Last.OK {
		t.Errorf("settings = %+v", s)
	}
	// a blank URL keeps the stored one; nil keeps the secret; "" clears it
	must(t, o.SetAlerts(ctx, "", nil))
	empty := ""
	must(t, o.SetAlerts(ctx, "", &empty))
	must(t, o.TestAlert(ctx))
	if x := h.next(t); x.sig != "" {
		t.Errorf("unsigned alert has %q", x.sig)
	}
	if err := o.SetAlerts(ctx, "ftp://x", nil); err == nil {
		t.Error("ftp URL taken")
	}
	must(t, o.ClearAlerts(ctx))
	if s, _ := o.AlertSettings(ctx); s.URL != "" || s.SecretSet || s.Last != nil {
		t.Errorf("after clear = %+v", s)
	}
}

// 5xx is retried with backoff; other 4xx is not, and the failure is recorded.
func TestAlertRetries(t *testing.T) {
	ctx := context.Background()
	alertBackoff = time.Millisecond
	t.Cleanup(func() { alertBackoff = 2 * time.Second })
	o := newWorld(t).orch
	h := newHook(t, 500, 503)
	alertsTo(t, o, h, "")
	must(t, o.sendAlert(ctx, Alert{Event: "x"}, 4))
	if n := len(h.got); n != 3 {
		t.Errorf("%d posts for two 5xx, want 3", n)
	}
	for len(h.got) > 0 {
		<-h.got
	}
	h.codes = []int{400}
	if err := o.sendAlert(ctx, Alert{Event: "x"}, 4); err == nil {
		t.Fatal("400 = nil")
	}
	if n := len(h.got); n != 1 {
		t.Errorf("%d posts for a 400, want 1", n)
	}
	s, _ := o.AlertSettings(ctx)
	if s.Last == nil || s.Last.OK || s.Last.Reason != "HTTP 400" {
		t.Errorf("last = %+v", s.Last)
	}
	if err := o.TestAlert(ctx); err != nil {
		t.Errorf("test after = %v", err)
	}
}

// A worker that drops alerts once as it goes offline; its first join says
// nothing.
func TestNodeOfflineAlertsOnce(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	h := newHook(t)
	alertsTo(t, o, h, "")
	_, srv, _ := workerNodeWith(t, o, "w1", node.StatusJoined, worker.Config{})
	must(t, o.refreshNodeClients(ctx))
	srv.Close()
	for range 4 {
		must(t, o.refreshNodeClients(ctx))
	}
	got := h.events(t, o)
	if len(got) != 1 || got[0] != AlertNodeOffline+":"+SevCritical {
		t.Errorf("events = %v", got)
	}
}

func TestNodeEvent(t *testing.T) {
	for _, c := range []struct {
		prev, next, want string
	}{
		{node.StatusOnline, node.StatusOffline, AlertNodeOffline},
		{node.StatusOffline, node.StatusOnline, AlertNodeOnline},
		{node.StatusJoined, node.StatusOnline, ""},
		{node.StatusOffline, node.StatusOffline, ""},
	} {
		a, ok := nodeEvent("w1", c.prev, c.next)
		if ok != (c.want != "") || a.Event != c.want {
			t.Errorf("%s -> %s = %q %v", c.prev, c.next, a.Event, ok)
		}
	}
}

// Disk low fires once per crossing and resets on recovery; a disk the node
// does not report changes nothing.
func TestDiskAlertOncePerCrossing(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	h := newHook(t)
	alertsTo(t, o, h, "")
	n := store.Node{ID: "n1", Name: "n1"}
	const g = 1 << 30
	steps := []struct {
		free, total uint64
		fires       int
	}{
		{50 * g, 100 * g, 0},
		{5 * g, 100 * g, 1}, // under 10%
		{4 * g, 100 * g, 0},
		{0, 0, 0}, // not reported
		{3 * g, 100 * g, 0},
		{50 * g, 100 * g, 0}, // recovered
		{g / 2, 2 * g, 1},    // under 1 GB
	}
	for i, s := range steps {
		o.diskAlert(ctx, n, s.free, s.total)
		if got := h.events(t, o); len(got) != s.fires {
			t.Errorf("step %d: %v", i, got)
		}
	}
}

// A cert fires once at 14 days, once at 3, and again after a renewal.
func TestCertAlertStages(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	h := newHook(t)
	alertsTo(t, o, h, "")
	now := time.Now()
	day := 24 * time.Hour
	for i, s := range []struct {
		left time.Duration
		want string
	}{
		{20 * day, ""},
		{10 * day, SevWarning},
		{9 * day, ""},
		{2 * day, SevCritical},
		{-day, ""}, // expired: still the 3 day stage
		{90 * day, ""},
		{10 * day, SevWarning},
	} {
		o.certAlert(ctx, "x", "ctl", "test certificate", now.Add(s.left), now)
		got := h.events(t, o)
		want := 0
		if s.want != "" {
			want = 1
		}
		if len(got) != want || (want == 1 && got[0] != AlertCertExpiring+":"+s.want) {
			t.Errorf("step %d: %v", i, got)
		}
	}
}

// A scheduled backup that fails alerts; a manual one and a skip do not.
func TestScheduledBackupFailureAlerts(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	h := newHook(t)
	alertsTo(t, o, h, "")
	run := func(p backupJob) {
		b, _ := json.Marshal(p)
		_ = o.handlers()[kindBackup](ctx, &jobs.Run{Job: store.Job{Payload: string(b)}, Log: io.Discard})
	}
	run(backupJob{VolumeID: "gone"})
	if got := h.events(t, o); len(got) != 0 {
		t.Errorf("manual: %v", got)
	}
	run(backupJob{VolumeID: "gone", ScheduleID: "s1"})
	if got := h.events(t, o); len(got) != 1 || got[0] != AlertBackupFailed+":"+SevCritical {
		t.Errorf("scheduled: %v", got)
	}
	o.backupResult(ctx, "x", "", "volume x", fmt.Errorf("%w: node offline", jobs.ErrSkipped))
	o.backupResult(ctx, "x", "", "volume x", context.Canceled)
	if got := h.events(t, o); len(got) != 0 {
		t.Errorf("skipped: %v", got)
	}
}

// A failing schedule alerts once, then after backupRemind; a success resets
// it and a skip changes nothing. The state is a row, so a restart keeps it.
func TestBackupFailedDebounce(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	h := newHook(t)
	alertsTo(t, o, h, "")
	fail := errors.New("s3 down")
	step := func(name string, err error, want int) {
		t.Helper()
		o.backupResult(ctx, "schedule.s1", "", "volume x", err)
		if got := h.events(t, o); len(got) != want {
			t.Errorf("%s: %v, want %d", name, got, want)
		}
	}
	step("first failure", fail, 1)
	step("again", fail, 0)
	step("skip", fmt.Errorf("%w: offline", jobs.ErrSkipped), 0)
	step("still", fail, 0)
	o.backupResult(ctx, "schedule.s2", "", "volume y", fail) // another schedule has its own state
	if got := h.events(t, o); len(got) != 1 {
		t.Errorf("other schedule: %v", got)
	}
	step("success", nil, 0)
	step("fails again", fail, 1)
	must(t, o.store.Settings.Set(ctx, "alert.backup.schedule.s1", time.Now().Add(-backupRemind-time.Hour).UTC().Format(time.RFC3339)))
	o.backupResult(ctx, "schedule.s1", "", "volume x", fail)
	if x := h.next(t); x.a.Event != AlertBackupFailed || !strings.Contains(x.a.Message, "still fails") {
		t.Errorf("reminder: %+v", x.a)
	}
	step("after reminder", fail, 0)
}

// A long message keeps its head and its tail, at most 300 characters.
func TestOneLineCutsTheMiddle(t *testing.T) {
	s := "HEAD " + strings.Repeat("x ", 400) + "\n the real cause TAIL"
	got := oneLine(s)
	if n := len([]rune(got)); n > 300 {
		t.Errorf("%d runes", n)
	}
	if !strings.HasPrefix(got, "HEAD ") || !strings.HasSuffix(got, "the real cause TAIL") || !strings.Contains(got, "…") {
		t.Errorf("%q", got)
	}
	if short := "a\nb"; oneLine(short) != "a b" {
		t.Errorf("short: %q", oneLine(short))
	}
}

// Three restarts inside ten minutes fire container.crashloop once; it
// clears with container.recovered when the window passes quietly.
func TestCrashloopAlertFiresAndClears(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	h := newHook(t)
	alertsTo(t, w.orch, h, "")
	w.tile(t, "worker", true)
	now := time.Now()
	steps := []struct {
		at       time.Duration
		restarts int
		want     []string
	}{
		{0, 1, nil},
		{time.Minute, 2, nil},
		{2 * time.Minute, 4, []string{AlertCrashloop + ":" + SevCritical}},
		{3 * time.Minute, 5, nil}, // still looping: no repeat
		{20 * time.Minute, 5, []string{AlertCrashOK + ":" + SevInfo}},
	}
	for i, s := range steps {
		w.fake.Details = map[string]docker.Detail{"c-worker": {ID: "c-worker", Running: true, RestartCount: s.restarts}}
		w.orch.containerAlerts(ctx, now.Add(s.at))
		if got := h.events(t, w.orch); !slices.Equal(got, s.want) {
			t.Errorf("step %d: %v, want %v", i, got, s.want)
		}
	}
}

// An OOM kill fires memory.low once per kill.
func TestOOMKillAlerts(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	h := newHook(t)
	alertsTo(t, w.orch, h, "")
	w.tile(t, "api", true)
	for i, s := range []struct {
		oom      bool
		restarts int
		fires    int
	}{{false, 0, 0}, {true, 1, 1}, {true, 1, 0}, {false, 1, 0}, {true, 2, 1}} {
		w.fake.Details = map[string]docker.Detail{"c-api": {ID: "c-api", Running: true, OOMKilled: s.oom, RestartCount: s.restarts}}
		w.orch.containerAlerts(ctx, time.Now())
		got := h.events(t, w.orch)
		if len(got) != s.fires || (s.fires == 1 && got[0] != AlertMemoryLow+":"+SevCritical) {
			t.Errorf("step %d: %v", i, got)
		}
	}
}

// Low memory must hold for five minutes before memory.low; recovery says so.
func TestMemoryAlertNeedsFiveMinutes(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	h := newHook(t)
	alertsTo(t, o, h, "")
	n := store.Node{ID: "n1", Name: "n1"}
	const g = 1 << 30
	now := time.Now()
	for i, s := range []struct {
		at    time.Duration
		free  uint64
		fires []string
	}{
		{0, 5 * g, nil}, // 5% of 100
		{4 * time.Minute, 5 * g, nil},
		{6 * time.Minute, 5 * g, []string{AlertMemoryLow + ":" + SevWarning}},
		{7 * time.Minute, 4 * g, nil},
		{8 * time.Minute, 50 * g, []string{AlertMemoryOK + ":" + SevInfo}},
		{9 * time.Minute, 5 * g, nil}, // the clock restarts
		{10 * time.Minute, 0, nil},    // not reported
	} {
		o.memAlert(ctx, n, s.free, 100*g, now.Add(s.at))
		if got := h.events(t, o); !slices.Equal(got, s.fires) {
			t.Errorf("step %d: %v, want %v", i, got, s.fires)
		}
	}
}

// The heartbeat URL is GET once a minute while set, and not when empty.
func TestHeartbeatPingsOnSchedule(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			hits++
		}
	}))
	t.Cleanup(srv.Close)
	now := time.Now()
	o.heartbeat(ctx, now) // off
	must(t, o.SetHeartbeat(ctx, srv.URL+"/ping/token"))
	for _, d := range []time.Duration{0, 30 * time.Second, time.Minute, 90 * time.Second, 2 * time.Minute} {
		o.heartbeat(ctx, now.Add(d))
	}
	if hits != 3 { // 0, 1m, 2m
		t.Errorf("pings = %d, want 3", hits)
	}
	if s, err := o.AlertSettings(ctx); err != nil || strings.Contains(s.Heartbeat, "token") || s.Heartbeat == "" {
		t.Errorf("settings = %q, %v", s.Heartbeat, err)
	}
	if err := o.SetHeartbeat(ctx, "ftp://x"); err == nil {
		t.Error("a non-http URL was accepted")
	}
}
