package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/logs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type memLogs struct {
	mu        sync.Mutex
	ls        []logs.Line
	newest    time.Time
	newestErr error
}

func (m *memLogs) Write(_ context.Context, ls []logs.Line) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ls = append(m.ls, ls...)
	return nil
}

func (m *memLogs) Newest(context.Context, time.Time) (time.Time, error) {
	return m.newest, m.newestErr
}

func (m *memLogs) Query(context.Context, logs.Filter, time.Time, time.Time, int) ([]logs.Line, error) {
	return nil, nil
}

// streamDocker follows like a real daemon: it sends its lines, then holds
// the stream open until the follower is cancelled.
type streamDocker struct {
	*dockerfake.Fake
	mu      sync.Mutex
	started map[string]int
	since   map[string]time.Time
	ended   chan string
}

func (d *streamDocker) StreamLogs(ctx context.Context, id string, _ int) (<-chan string, func(), error) {
	d.mu.Lock()
	d.started[id]++
	if t, ok := docker.LogsSinceFrom(ctx); ok {
		d.since[id] = t
	}
	d.mu.Unlock()
	ch := make(chan string, 4)
	ch <- "O 2026-10-21T10:00:00.5Z hello"
	ch <- "E 2026-10-21T10:00:01Z boom"
	go func() {
		<-ctx.Done()
		d.ended <- id
	}()
	return ch, func() {}, nil
}

func (d *streamDocker) count(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.started[id]
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 500 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

func (o *Orchestrator) bufLen() int {
	o.logship.mu.Lock()
	defer o.logship.mu.Unlock()
	return len(o.logship.buf)
}

// The tick follows each running replica once (not the pause or run
// containers), lines land enriched, a gone container's follower ends, and
// turning logs off ends the rest.
func TestLogshipFollow(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	mem := &memLogs{}
	o.logship.store = mem
	sd := &streamDocker{Fake: w.fake, started: map[string]int{}, since: map[string]time.Time{}, ended: make(chan string, 8)}
	o.docker = sd
	must(t, o.SetSettings(ctx, map[string]string{"observe_enabled": "true", "observe_logs_enabled": "true"}))

	api := w.tile(t, "api", true)
	w.tile(t, "web", true)
	for i := range w.fake.Containers {
		w.fake.Containers[i].Labels[docker.LabelManaged] = "true"
	}
	w.fake.Containers = append(w.fake.Containers,
		docker.Container{ID: "pause", State: "running", Labels: map[string]string{docker.LabelManaged: "true", tile.LabelTile: api.ID, tile.LabelRole: "pause"}},
		docker.Container{ID: "run1", State: "running", Labels: map[string]string{docker.LabelManaged: "true", tile.LabelTile: api.ID, tile.LabelRole: "run"}})

	must(t, o.logshipTick(ctx))
	waitFor(t, "four lines", func() bool { return o.bufLen() == 4 })
	must(t, o.logshipTick(ctx)) // already followed: no second stream
	if sd.count("c-api") != 1 || sd.count("pause") != 0 || sd.count("run1") != 0 {
		t.Errorf("streams: api %d pause %d run %d", sd.count("c-api"), sd.count("pause"), sd.count("run1"))
	}
	o.flushLogs(ctx)
	mem.mu.Lock()
	var got logs.Line
	for _, l := range mem.ls {
		if l.TileID == api.ID && l.Stream == "stderr" {
			got = l
		}
	}
	mem.mu.Unlock()
	if got.Msg != "boom" || got.Org != "acme" || got.OrgID != w.org || got.Stack != "shop" || got.Env != "dev" ||
		got.EnvID != w.env || got.Tile != "api" || got.Kind != tile.Image || got.Source != logs.SourceApp || got.Container != "c-api" ||
		!got.Time.Equal(time.Date(2026, 10, 21, 10, 0, 1, 0, time.UTC)) {
		t.Errorf("line %+v", got)
	}

	// c-web is gone: its follower is cancelled, api's keeps going.
	w.fake.Containers = w.fake.Containers[:1]
	must(t, o.logshipTick(ctx))
	select {
	case id := <-sd.ended:
		if id != "c-web" {
			t.Errorf("ended %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gone container's follower never ended")
	}
	must(t, o.SetSettings(ctx, map[string]string{"observe_logs_enabled": "false"}))
	must(t, o.logshipTick(ctx))
	select {
	case id := <-sd.ended:
		if id != "c-api" {
			t.Errorf("ended %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logs off left a follower")
	}
}

// A full buffer drops its oldest line and counts it.
func TestLogshipBounded(t *testing.T) {
	o := &Orchestrator{}
	for i := range logBufMax + 3 {
		o.logship.add(logs.Line{Msg: string(rune('a' + i%26)), Node: string(rune(i))})
	}
	if o.bufLen() != logBufMax || o.LogsDropped() != 3 {
		t.Errorf("held %d dropped %d", o.bufLen(), o.LogsDropped())
	}
	if o.logship.buf[0].Node != string(rune(3)) {
		t.Error("the newest lines were dropped, not the oldest")
	}
}

const caddyLine = `{"level":"info","ts":1729500000.1,"logger":"http.log.access","msg":"handled request","request":{"remote_ip":"127.0.0.1","remote_port":"39678","client_ip":"127.0.0.1","proto":"HTTP/1.1","method":"GET","host":"app.example.com","uri":"/x"},"bytes_read":0,"user_id":"","duration":0.0013,"size":2,"status":200,"user_agent":"tester/1"}`

func TestParseAccess(t *testing.T) {
	host, f, ok := parseAccess(caddyLine)
	want := map[string]string{"http.method": "GET", "http.path": "/x", "http.status": "200",
		"http.duration_ms": "1.3", "http.bytes": "2", "http.ua": "tester/1", "http.ip": "127.0.0.1"}
	if !ok || host != "app.example.com" || len(f) != len(want) {
		t.Fatalf("host %q fields %v ok %v", host, f, ok)
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("%s = %q, want %q", k, f[k], v)
		}
	}
	for _, s := range []string{`{"level":"info","logger":"tls.obtain","msg":"x"}`, `plain text http.log.access`, `{"logger":"http.log.access"`} {
		if _, _, ok := parseAccess(s); ok {
			t.Errorf("%q taken for an access line", s)
		}
	}
	// Headers on and no top-level user_agent: the header gives the ua. Ip off:
	// no ip fields.
	_, f, _ = parseAccess(`{"logger":"http.log.access","request":{"host":"a.b","headers":{"User-Agent":["curl/8"]}},"status":200}`)
	if f["http.ua"] != "curl/8" || f["http.ip"] != "" {
		t.Errorf("header ua: %v", f)
	}
	// A line with fewer fields gives fewer.
	if _, f, ok := parseAccess(`{"logger":"http.log.access","request":{"host":"a.b"},"status":200}`); !ok || len(f) != 1 || f["http.status"] != "200" {
		t.Errorf("sparse line: %v %v", f, ok)
	}
}

// An access line lands on the tile its host maps to, with source=access; a
// host that maps to none is dropped. A run's line carries its run id.
func TestShipAccessAndRun(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	o := w.orch
	tl := w.tile(t, "api", false)
	o.logship.tileOf = tileOfHost([]store.Domain{{Host: "app.example.com", TileID: tl.ID}})
	n := store.Node{Name: "edge"}
	ts := time.Unix(1792576800, 0)
	o.shipAccess(ctx, n, docker.Container{Name: "stackr-proxy"}, ts, caddyLine)
	o.shipAccess(ctx, n, docker.Container{Name: "stackr-proxy"}, ts, `{"logger":"http.log.access","request":{"host":"other.example.com"},"status":200}`)
	o.shipAccess(ctx, n, docker.Container{Name: "stackr-proxy"}, ts, `{"logger":"admin.api","msg":"x"}`)
	if o.bufLen() != 1 {
		t.Fatalf("buffered %d lines", o.bufLen())
	}
	l := o.logship.buf[0]
	if l.Source != logs.SourceAccess || l.TileID != tl.ID || l.Org != "acme" || l.HTTP["http.status"] != "200" || l.Node != "edge" || l.Container != "stackr-proxy" {
		t.Errorf("line %+v", l)
	}

	o.shipRun(store.Tile{ID: tl.ID}, "run-1", "O 2026-10-21T10:00:00Z step 1")
	if o.bufLen() != 1 {
		t.Error("a run line was taken with logs off")
	}
	o.logship.on.Store(true)
	o.shipRun(store.Tile{ID: tl.ID}, "run-1", "E 2026-10-21T10:00:00Z step 1")
	r := o.logship.buf[1]
	if r.Source != logs.SourceRun || r.RunID != "run-1" || r.Msg != "step 1" || r.Stream != "stderr" || r.TileID != tl.ID {
		t.Errorf("run line %+v", r)
	}
}

// The first tick after a start follows from the newest line the store has
// (Docker is the buffer across a panel outage); an empty or unreachable
// store means from now.
func TestLogshipResume(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		newest time.Time
		err    error
		want   bool
	}{
		"newest line": {newest: time.Now().Add(-time.Hour).Truncate(time.Second), want: true},
		"empty store": {},
		"unreachable": {err: errors.New("down")},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			o := w.orch
			o.logship.store = &memLogs{newest: c.newest, newestErr: c.err}
			sd := &streamDocker{Fake: w.fake, started: map[string]int{}, since: map[string]time.Time{}, ended: make(chan string, 8)}
			o.docker = sd
			must(t, o.SetSettings(ctx, map[string]string{"observe_enabled": "true", "observe_logs_enabled": "true"}))
			w.tile(t, "api", true)
			w.fake.Containers[0].Labels[docker.LabelManaged] = "true"
			must(t, o.logshipTick(ctx))
			waitFor(t, "a stream", func() bool { return sd.count("c-api") == 1 })
			sd.mu.Lock()
			got, ok := sd.since["c-api"]
			sd.mu.Unlock()
			if ok != c.want || (ok && !got.Equal(c.newest)) {
				t.Errorf("since %v (set %v), want %v (set %v)", got, ok, c.newest, c.want)
			}
		})
	}
}
