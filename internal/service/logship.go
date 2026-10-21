package service

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/logs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Log shipping: the controller pulls. Every 30 s it lists the managed
// containers on each node through the Docker API it already reaches, follows
// the new ones and drops the followers of the gone. Lines are enriched from
// the DB, batched, and written to the log store (VictoriaLogs). Nothing runs
// on workers. Proxy containers are followed too, keeping only Caddy's access
// lines; a run's lines arrive through shipRun.

const (
	logBufMax     = 10_000 // lines held when the store is slow or down
	logBatch      = 1000   // lines that flush early
	logFlushEvery = 2 * time.Second
	logMetaTTL    = 5 * time.Minute // a rename or delete shows up within this
)

type logShip struct {
	on      atomic.Bool   // logs are kept (observe and observe_logs_enabled)
	dropped atomic.Uint64 // lines dropped for a full buffer since the panel started
	down    atomic.Bool   // the last write failed (logged once per change)

	baseOnce sync.Once
	base     context.Context // followers and the flush loop; ends with the panel
	wake     chan struct{}
	store    logs.Store // nil = VictoriaLogs; tests set it

	mu     sync.Mutex
	buf    []logs.Line
	follow map[string]context.CancelFunc // node/container id -> its follower
	last   map[string]time.Time          // key -> the newest time shipped (or started at)
	tickAt time.Time
	tileOf func(host string) string // host -> tile id, from the domain rows
	tileAt time.Time                // when tileOf was last read
	nodes  map[string]string        // node id -> name ("" = the controller), for run lines
	meta   map[string]logMeta
}

type logMeta struct {
	line logs.Line // the tile's org, stack, env, tile and kind
	at   time.Time
}

// LogsDropped is how many lines the bounded buffer has dropped.
func (o *Orchestrator) LogsDropped() uint64 { return o.logship.dropped.Load() }

func (o *Orchestrator) logStore() logs.Store {
	if o.logship.store != nil {
		return o.logship.store
	}
	return &logs.Victoria{URL: "http://observe", Client: observeClient(installspec.ObserveLogsSocket(o.cfg.DataDir))}
}

// logBase starts the flush loop on first use and returns the context the
// followers run under.
func (o *Orchestrator) logBase() context.Context {
	s := &o.logship
	s.baseOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-o.stopNodes; cancel() }()
		s.base, s.wake = ctx, make(chan struct{}, 1)
		go o.logLoop(ctx)
	})
	return s.base
}

// add buffers a line; a full buffer drops its oldest.
func (s *logShip) add(l logs.Line) {
	s.mu.Lock()
	if len(s.buf) >= logBufMax {
		s.buf = s.buf[1:]
		s.dropped.Add(1)
	}
	s.buf = append(s.buf, l)
	full := len(s.buf) >= logBatch
	s.mu.Unlock()
	if full && s.wake != nil {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (o *Orchestrator) logLoop(ctx context.Context) {
	t := time.NewTicker(logFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-o.logship.wake:
		}
		o.flushLogs(ctx)
	}
}

// flushLogs writes the buffer. A failed write puts the lines back in front
// (the bound still holds, counting what it has to drop).
func (o *Orchestrator) flushLogs(ctx context.Context) {
	s := &o.logship
	s.mu.Lock()
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := o.logStore().Write(wctx, batch)
	if down := err != nil; s.down.Swap(down) != down {
		if down {
			slog.Warn("observe: log write failed", "err", err)
		} else {
			slog.Info("observe: log writes back")
		}
	}
	if err == nil {
		return
	}
	s.mu.Lock()
	s.buf = append(batch, s.buf...)
	if over := len(s.buf) - logBufMax; over > 0 {
		s.buf = s.buf[over:]
		s.dropped.Add(uint64(over))
	}
	s.mu.Unlock()
}

// stopAll ends every follower and forgets what was buffered.
func (s *logShip) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.follow {
		cancel()
	}
	s.follow, s.last, s.buf, s.tickAt = nil, nil, nil, time.Time{}
}

// logshipTick reconciles the followers with the containers that exist now.
func (o *Orchestrator) logshipTick(ctx context.Context) error {
	s := &o.logship
	k, err := o.observeKnobs(ctx)
	if err != nil {
		return err
	}
	s.on.Store(k.logs)
	if !k.logs {
		s.stopAll()
		return nil
	}
	base := o.logBase()
	o.loadTileOf(ctx)
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, n := range ns {
		names[n.ID] = n.Name
		if n.Role == node.Controller {
			names[""] = n.Name
		}
	}
	s.mu.Lock()
	s.nodes = names
	s.mu.Unlock()
	now := time.Now()
	s.mu.Lock()
	prev := s.tickAt
	first := prev.IsZero()
	if s.follow == nil {
		s.follow, s.last = map[string]context.CancelFunc{}, map[string]time.Time{}
	}
	s.mu.Unlock()

	if first {
		// Docker is the buffer: resume from the newest line the store has,
		// so what was logged while the panel was down is still collected.
		prev = o.logResume(ctx, now)
	}
	seen := map[string]bool{}
	unlisted := map[string]bool{} // nodes that did not answer: their followers stay
	for _, n := range ns {
		if n.Role != node.Controller && n.Status != node.StatusOnline {
			continue
		}
		lctx, cancel := context.WithTimeout(docker.WithNode(ctx, n.ID), 10*time.Second)
		tiles, err1 := o.docker.List(lctx, map[string]string{docker.LabelManaged: "true"})
		proxies, err2 := o.docker.List(lctx, map[string]string{installspec.LabelRole: "proxy"})
		cancel()
		if err1 != nil || err2 != nil {
			unlisted[n.ID] = true
			continue
		}
		for _, c := range tiles {
			if c.State != "running" || c.Labels["stackr.tile"] == "" || c.Labels["stackr.role"] != "replica" {
				continue // pause holds the VIP; runs ship through their own run
			}
			key := n.ID + "/" + c.ID
			seen[key] = true
			o.followLog(base, key, n, c, false, prev)
		}
		for _, c := range proxies {
			if c.State != "running" {
				continue
			}
			key := n.ID + "/" + c.ID
			seen[key] = true
			o.followLog(base, key, n, c, true, prev)
		}
	}
	s.mu.Lock()
	for key, cancel := range s.follow {
		if !seen[key] && !unlisted[strings.SplitN(key, "/", 2)[0]] {
			cancel()
			delete(s.follow, key)
			delete(s.last, key)
		}
	}
	s.tickAt = now
	s.mu.Unlock()
	return nil
}

// loadTileOf reads the host -> tile map from the domain rows.
func (o *Orchestrator) loadTileOf(ctx context.Context) {
	ds, err := o.store.Domains.List(ctx)
	if err != nil {
		return
	}
	s := &o.logship
	s.mu.Lock()
	s.tileOf, s.tileAt = tileOfHost(ds), time.Now()
	s.mu.Unlock()
}

// tileOfReload is how often a host that maps to no tile may re-read the
// domains: a domain added since the last tick is found within it, and the
// panel's own host (never a tile) costs one read per period, not per request.
const tileOfReload = 5 * time.Second

// logResumeMax caps how far back a restart replays.
const logResumeMax = 24 * time.Hour

// logResume is where the first tick starts following: the store's newest
// line within logResumeMax, else zero (follow from now).
func (o *Orchestrator) logResume(ctx context.Context, now time.Time) time.Time {
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t, err := o.logStore().Newest(rctx, now.Add(-logResumeMax))
	if err != nil {
		slog.Warn("observe: log resume point unknown, following from now", "err", err)
		return time.Time{}
	}
	return t
}

// followLog starts a follower of container c unless it has one. A container
// seen before (or one that appeared since the last tick) is read from the
// last time shipped (or that tick); the first tick reads from now.
// ponytail: one follower goroutine and one Docker stream per container; fine
// to hundreds. Past that, a shared reader per node or a worker-side shipper.
func (o *Orchestrator) followLog(base context.Context, key string, n store.Node, c docker.Container, proxy bool, prevTick time.Time) {
	s := &o.logship
	s.mu.Lock()
	if _, ok := s.follow[key]; ok {
		s.mu.Unlock()
		return
	}
	since, had := s.last[key]
	if had {
		since = since.Add(time.Nanosecond)
	} else {
		since, s.last[key] = prevTick, time.Now()
	}
	fctx, cancel := context.WithCancel(docker.WithNode(base, n.ID))
	s.follow[key] = cancel
	s.mu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			s.mu.Lock()
			delete(s.follow, key) // a stopped container is followed again when it runs
			s.mu.Unlock()
		}()
		sctx, tail := fctx, 0
		if !since.IsZero() {
			sctx, tail = docker.WithLogsSince(fctx, since), -1
		}
		lines, stop, err := o.docker.StreamLogs(sctx, c.ID, tail)
		if err != nil {
			slog.Debug("observe: log follow failed", "container", c.Name, "err", err)
			return
		}
		defer stop()
		for {
			select {
			case <-fctx.Done():
				return
			case raw, ok := <-lines:
				if !ok {
					return
				}
				stream, ts, msg := parseDockerLine(raw)
				s.mu.Lock()
				if ts.After(s.last[key]) {
					s.last[key] = ts
				}
				s.mu.Unlock()
				if proxy {
					o.shipAccess(fctx, n, c, ts, msg)
					continue
				}
				l, ok := o.logMeta(fctx, c.Labels["stackr.tile"])
				if !ok {
					continue
				}
				l.Time, l.Msg, l.Stream, l.Source = ts, msg, stream, logs.SourceApp
				l.Node, l.Container = n.Name, c.Name
				s.add(l)
			}
		}
	}()
}

// parseDockerLine splits StreamLogs' "O <rfc3339 time> message" (E = stderr).
func parseDockerLine(s string) (stream string, t time.Time, msg string) {
	stream = "stdout"
	if strings.HasPrefix(s, "E ") {
		stream = "stderr"
	}
	if len(s) >= 2 {
		s = s[2:]
	}
	ts, rest, _ := strings.Cut(s, " ")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return stream, time.Now(), s
	}
	return stream, t, rest
}

// logMeta is a tile's org, stack, env, tile and kind for a line, cached for
// logMetaTTL so a rename or delete shows up soon. ok is false for a tile the
// DB no longer has.
func (o *Orchestrator) logMeta(ctx context.Context, tileID string) (logs.Line, bool) {
	s := &o.logship
	s.mu.Lock()
	if m, ok := s.meta[tileID]; ok && time.Since(m.at) < logMetaTTL {
		s.mu.Unlock()
		return m.line, true
	}
	s.mu.Unlock()
	t, err := o.store.Tiles.Get(ctx, tileID)
	if err != nil {
		return logs.Line{}, false
	}
	l := logs.Line{TileID: t.ID, Tile: t.Name, Kind: t.Kind, StackID: t.StackID, EnvID: t.EnvironmentID}
	if st, err := o.store.Stacks.Get(ctx, t.StackID); err == nil {
		l.Stack, l.OrgID = st.Name, st.OrgID
		if org, err := o.store.Orgs.Get(ctx, st.OrgID); err == nil {
			l.Org = org.Name
		}
	}
	if e, err := o.store.Environments.Get(ctx, t.EnvironmentID); err == nil {
		l.Env = e.Name
	}
	s.mu.Lock()
	if s.meta == nil {
		s.meta = map[string]logMeta{}
	}
	s.meta[tileID] = logMeta{l, time.Now()}
	s.mu.Unlock()
	return l, true
}

// shipRun hands one line of a run (as RunOnce copies it) to the shipper;
// ctx carries the node the run is on.
func (o *Orchestrator) shipRun(ctx context.Context, t store.Tile, runID, container, raw string) {
	if !o.logship.on.Load() {
		return
	}
	l, ok := o.logMeta(context.WithoutCancel(ctx), t.ID)
	if !ok {
		return
	}
	l.Stream, l.Time, l.Msg = parseDockerLine(raw)
	l.Source, l.RunID, l.Container = logs.SourceRun, runID, container
	id, _ := docker.NodeFrom(ctx)
	o.logship.mu.Lock()
	l.Node = cmp.Or(o.logship.nodes[id], id)
	o.logship.mu.Unlock()
	o.logship.add(l)
}

// caddyAccess is the part of a Caddy access log line the shipper reads.
type caddyAccess struct {
	Logger   string  `json:"logger"`
	Status   int     `json:"status"`
	Duration float64 `json:"duration"` // seconds
	Size     int64   `json:"size"`
	// UserAgent is added by Caddy's log_append; request.headers is absent
	// unless observe_access_headers is on.
	UserAgent string `json:"user_agent"`
	Request   struct {
		RemoteIP string              `json:"remote_ip"`
		ClientIP string              `json:"client_ip"`
		Method   string              `json:"method"`
		Host     string              `json:"host"`
		URI      string              `json:"uri"`
		Headers  map[string][]string `json:"headers"`
	} `json:"request"`
}

// parseAccess reads a proxy's stdout line: ok only for a Caddy http.log.access
// JSON line. Fields that are absent are left out of the result.
func parseAccess(msg string) (host string, f map[string]string, ok bool) {
	if !strings.Contains(msg, "http.log.access") {
		return "", nil, false
	}
	var a caddyAccess
	if json.Unmarshal([]byte(msg), &a) != nil || !strings.HasPrefix(a.Logger, "http.log.access") {
		return "", nil, false
	}
	f = map[string]string{}
	set := func(k, v string) {
		if v != "" {
			f["http."+k] = v
		}
	}
	set("method", a.Request.Method)
	set("path", a.Request.URI)
	if a.Status != 0 {
		set("status", strconv.Itoa(a.Status))
	}
	if a.Duration != 0 {
		set("duration_ms", strconv.FormatFloat(a.Duration*1000, 'f', -1, 64))
	}
	if a.Size != 0 {
		set("bytes", strconv.FormatInt(a.Size, 10))
	}
	set("ip", a.Request.RemoteIP)
	if a.Request.ClientIP != "" {
		set("ip", a.Request.ClientIP)
	}
	set("ua", a.UserAgent)
	for k, v := range a.Request.Headers {
		if a.UserAgent != "" {
			break
		}
		if strings.EqualFold(k, "User-Agent") && len(v) > 0 {
			set("ua", v[0])
		}
	}
	host = strings.ToLower(a.Request.Host)
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.HasSuffix(host, "]") {
		host = host[:i]
	}
	return host, f, true
}

// shipAccess writes a proxy's access line for the tile its host maps to.
// A host that maps to no tile has no org to be read under and is dropped.
func (o *Orchestrator) shipAccess(ctx context.Context, n store.Node, c docker.Container, ts time.Time, msg string) {
	host, f, ok := parseAccess(msg)
	if !ok {
		return
	}
	s := &o.logship
	s.mu.Lock()
	tileOf := s.tileOf
	s.mu.Unlock()
	id := ""
	if tileOf != nil {
		id = tileOf(host)
	}
	if id == "" {
		s.mu.Lock()
		stale := time.Since(s.tileAt) >= tileOfReload
		s.mu.Unlock()
		if !stale {
			return
		}
		o.loadTileOf(ctx)
		s.mu.Lock()
		tileOf = s.tileOf
		s.mu.Unlock()
		if tileOf == nil {
			return
		}
		if id = tileOf(host); id == "" {
			return
		}
	}
	l, ok := o.logMeta(ctx, id)
	if !ok {
		return
	}
	l.Time, l.Stream, l.Source, l.HTTP = ts, "stdout", logs.SourceAccess, f
	l.Node, l.Container = n.Name, c.Name
	l.Msg = strings.TrimSpace(f["http.method"] + " " + f["http.path"] + " " + f["http.status"])
	s.add(l)
}
