package agent

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
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/hostmetrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

// Server is the agent side: a narrow HTTP surface over this node's own
// docker socket, plus the two things that are not socket calls at all, the
// /proc sampler and the volume move.
type Server struct {
	RT  *runtime.Runtime
	Key string
	// Version is the binary's build version, echoed on every reply so the
	// panel can refuse a mismatch and show the node as "agent outdated".
	Version string
	// PanelURL is where samples are posted, the panel's service name on the
	// stkr overlay.
	PanelURL string
	// NodeID is this node's swarm ID, so the panel knows whose sample it is.
	NodeID string
}

// Handler builds the agent's routes. Every one of them is authenticated by
// the shared runtime key; there is no unauthenticated surface except the
// health probe swarm itself uses.
func (s *Server) Handler() http.Handler {
	// api holds every authenticated route. Auth wraps the whole mux rather
	// than each route, so the method check cannot answer before it: with
	// per-route wrapping a keyless GET of a POST route got 405 and a keyless
	// GET of a missing one got 404, which let anyone already on the overlay
	// map the surface without a key. Unknown
	// paths now answer 401, which is the point.
	api := http.NewServeMux()

	api.HandleFunc("POST "+PathInfo, s.info)
	api.HandleFunc("POST "+PathInspect, s.inspect)
	api.HandleFunc("POST "+PathStats, s.stats)
	api.HandleFunc("POST "+PathHealth, s.health)
	api.HandleFunc("POST "+PathPause, s.pause)
	api.HandleFunc("POST "+PathPrune, s.prune)
	api.HandleFunc("POST "+PathVolumes, s.volumes)
	api.HandleFunc("POST "+PathContainers, s.containers)
	api.HandleFunc("POST "+PathContainerAction, s.containerAction)
	api.HandleFunc("POST "+PathContainerSystem, s.containerSystem)

	api.HandleFunc("POST "+PathVolumeList, s.volumeList)
	api.HandleFunc("POST "+PathVolumeDelete, s.volumeDelete)
	api.HandleFunc("POST "+PathVolumeCreate, s.volumeCreate)
	api.HandleFunc("POST "+PathVolumeRemove, s.volumeRemove)
	api.HandleFunc("POST "+PathVolumeSize, s.volumeSize)
	api.HandleFunc("GET "+PathVolumeRead, s.volumeRead)
	api.HandleFunc("POST "+PathVolumeWrite, s.volumeWrite)
	api.HandleFunc("GET "+PathVolumeTar, s.volumeTar)
	api.HandleFunc("POST "+PathVolumeUntar, s.volumeUntar)

	api.HandleFunc("GET "+PathExec, s.exec)
	api.HandleFunc("POST "+PathExecRun, s.execRun)
	api.HandleFunc("POST "+PathExecShell, s.execShell)
	api.HandleFunc("GET "+PathLogs, s.logs)

	api.HandleFunc("POST "+PathMoveReceive, s.moveReceive)
	api.HandleFunc("POST "+PathMoveSend, s.moveSend)
	api.HandleFunc("POST "+PathMoveClose, s.moveClose)

	mux := http.NewServeMux()
	// Swarm's own liveness check. Deliberately outside auth: it proves the
	// process is up, and says nothing else. More specific than "/", so it
	// wins the pattern match.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", s.auth(api.ServeHTTP))
	return mux
}

// auth checks the shared runtime key and stamps the version on the reply.
//
// The key proves the caller is stackr, not which stackr: every agent task
// holds it, so root on any node can call every other agent as the panel.
// That is a known ceiling, written down in
// docs/plans/31-node-agent-open-questions.md under who can read the key, and
// closing it is the hard-tenancy item in plan 30, not this step.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeader, s.Version)
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		// Length-independent compare is not worth it here: the key is a
		// 32-byte hex string of the same length on every call, and the
		// attacker has to already be on the overlay to make one.
		if s.Key == "" || got != s.Key {
			writeErr(w, http.StatusUnauthorized, errors.New("bad agent key"))
			return
		}
		next(w, r)
	}
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ErrResp{Error: err.Error()})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads a JSON request body into v.
func decode[T any](r *http.Request) (T, error) {
	var v T
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&v)
	return v, err
}

// --- plain calls ----------------------------------------------------------

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	host, err := s.RT.Info(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, InfoResp{Host: host, Distro: distro()})
}

// distro reads the host's pretty name. The panel cannot answer this for a
// worker and cannot answer it for itself either, its own container's
// /etc/os-release describes the image, not the machine, so the agent reads
// the host file the deploy mounts in.
func distro() string {
	for _, p := range []string{"/host/etc/os-release", "/etc/os-release"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				return strings.Trim(v, `"`)
			}
		}
	}
	return ""
}

func (s *Server) inspect(w http.ResponseWriter, r *http.Request) {
	req, err := decode[ContainerReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	d, err := s.RT.InspectContainer(r.Context(), req.Container)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, d)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	req, err := decode[ContainerReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := s.RT.Stats(r.Context(), req.Container)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	req, err := decode[ContainerReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := s.RT.HealthStatus(r.Context(), req.Container)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]string{"status": st})
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	req, err := decode[PauseReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Unpause {
		err = s.RT.UnpauseContainer(r.Context(), req.Container)
	} else {
		err = s.RT.PauseContainer(r.Context(), req.Container)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, struct{}{})
}

func (s *Server) prune(w http.ResponseWriter, r *http.Request) {
	out, err := s.RT.Prune(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, PruneResp{Output: out})
}

func (s *Server) volumes(w http.ResponseWriter, r *http.Request) {
	vs, err := s.RT.ListVolumes(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, vs)
}

func (s *Server) containers(w http.ResponseWriter, r *http.Request) {
	cs, err := s.RT.ListAll(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, cs)
}

func (s *Server) containerAction(w http.ResponseWriter, r *http.Request) {
	req, err := decode[ContainerActionReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	switch req.Action {
	case "start":
		err = s.RT.StartContainer(r.Context(), req.Container)
	case "stop":
		err = s.RT.StopContainer(r.Context(), req.Container)
	case "remove":
		err = s.RT.StopRemove(r.Context(), req.Container)
	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unknown action %q", req.Action))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, struct{}{})
}

// containerSystem answers whether a container is one of stackr's own. The
// panel asks before offering stop or remove, and the answer depends on labels
// only this node can read.
func (s *Server) containerSystem(w http.ResponseWriter, r *http.Request) {
	req, err := decode[ContainerReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, BoolResp{OK: s.RT.ContainerIsSystem(r.Context(), req.Container)})
}

// --- volume files ---------------------------------------------------------

func (s *Server) volumeList(w http.ResponseWriter, r *http.Request) {
	req, err := decode[VolumeReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	es, err := s.RT.ListVolumeFiles(r.Context(), req.Volume, req.Dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, es)
}

func (s *Server) volumeDelete(w http.ResponseWriter, r *http.Request) {
	req, err := decode[VolumeReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.RT.DeleteVolumeFile(r.Context(), req.Volume, req.File); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, struct{}{})
}

// volumeCreate makes a named volume on this node.
func (s *Server) volumeCreate(w http.ResponseWriter, r *http.Request) {
	req, err := decode[VolumeReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Driver first: docker returns the existing volume unchanged when the name
	// is taken, so a plain create ahead of an opts create would silently win
	// and the share would never be mounted.
	if req.Driver != "" {
		err = s.RT.CreateVolumeOpts(r.Context(), req.Volume, req.Driver, req.Opts)
	} else {
		err = s.RT.CreateVolume(r.Context(), req.Volume)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, struct{}{})
}

// volumeRemove destroys a named volume on this node, clearing the stopped
// containers that hold it first. Docker refuses while a *running* container
// has it mounted, and that refusal is the safety net the panel relies on, so
// it is passed straight back rather than forced.
//
// Only reached from Nodes.RemoveVolume, which is the node page's Delete
// button, so the purge is the operator's own decision arriving here.
func (s *Server) volumeRemove(w http.ResponseWriter, r *http.Request) {
	req, err := decode[VolumeReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.RT.PurgeVolume(r.Context(), req.Volume); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, struct{}{})
}

func (s *Server) volumeSize(w http.ResponseWriter, r *http.Request) {
	req, err := decode[VolumeReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := s.RT.VolumeSize(r.Context(), req.Volume)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, SizeResp{Bytes: n})
}

func (s *Server) volumeRead(w http.ResponseWriter, r *http.Request) {
	vol, file := r.URL.Query().Get("volume"), r.URL.Query().Get("file")
	rc, wait, err := s.RT.ReadVolumeFile(r.Context(), vol, file)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer func() { _ = rc.Close() }()
	// The status line is already sent by the time a read fails, so a failure
	// halfway can only be a truncated body. The trailer is what says so; the
	// panel's reader turns it back into the error a local read would give.
	w.Header().Set("Trailer", StreamErrorTrailer)
	w.WriteHeader(http.StatusOK)
	_, copyErr := io.Copy(w, rc)
	msg := ""
	if err := wait(); err != nil {
		slog.Error("agent: volume read", "volume", vol, "file", file, "error", err)
		msg = err.Error()
	} else if copyErr != nil {
		msg = copyErr.Error()
	}
	w.Header().Set(StreamErrorTrailer, msg)
}

func (s *Server) volumeWrite(w http.ResponseWriter, r *http.Request) {
	vol, file := r.URL.Query().Get("volume"), r.URL.Query().Get("file")
	if err := s.RT.WriteVolumeFile(r.Context(), vol, file, r.Body); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, struct{}{})
}

func (s *Server) volumeTar(w http.ResponseWriter, r *http.Request) {
	vol := r.URL.Query().Get("volume")
	w.Header().Set("Trailer", StreamErrorTrailer)
	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	msg := ""
	// The body has already started, so the failure can only be said in the
	// trailer. Logging it alone left the caller with a short archive and no
	// reason to doubt it.
	if err := s.RT.TarVolume(r.Context(), vol, w, r.URL.Query().Get("live") != ""); err != nil {
		slog.Error("agent: tar volume", "volume", vol, "error", err)
		msg = err.Error()
	}
	w.Header().Set(StreamErrorTrailer, msg)
}

func (s *Server) volumeUntar(w http.ResponseWriter, r *http.Request) {
	vol := r.URL.Query().Get("volume")
	if err := s.RT.UntarVolume(r.Context(), vol, r.Body); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, struct{}{})
}

// execRun runs one non-TTY exec: the request body is its stdin, the response
// body its stdout. This is how a database dump and a restore reach a node
// that is not the manager.
//
// The command's own failure comes back in a trailer, because by the time it
// fails the body has already started and no status code is left. A caller
// that ignores the trailer gets a truncated dump it believes in.
func (s *Server) execRun(w http.ResponseWriter, r *http.Request) {
	cid := r.URL.Query().Get("container")
	var cmd []string
	if raw := r.URL.Query().Get("cmd"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	var stdin io.Reader
	if r.ContentLength != 0 {
		stdin = r.Body
	}
	rd, wait, err := s.RT.ExecStream(r.Context(), cid, cmd, stdin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Trailer", ExecErrorTrailer)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, copyErr := io.Copy(w, rd)
	msg := ""
	if err := wait(); err != nil {
		msg = err.Error()
	} else if copyErr != nil {
		msg = copyErr.Error()
	}
	w.Header().Set(ExecErrorTrailer, msg)
}

// execShell runs one shell command in a container and waits for it.
func (s *Server) execShell(w http.ResponseWriter, r *http.Request) {
	req, err := decode[ExecShellReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.RT.ExecShell(r.Context(), req.Container, req.Command)
	resp := ExecShellResp{Output: out}
	if err != nil {
		resp.Error = err.Error()
	}
	writeJSON(w, resp)
}

// --- streams --------------------------------------------------------------

// exec bridges a websocket to a TTY exec in the container. Binary frames are
// raw bytes each way; a text frame is a resize, the same protocol the browser
// terminal already speaks (handlers/web/wsterm), so the panel can hand the
// two ends to each other without translating.
func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()

	// The request context dies with the handler's own deadline; an exec
	// outlives that by design. Cancel on return still tears it down.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	var cmd []string
	if raw := r.URL.Query().Get("cmd"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
			_ = ws.Close(websocket.StatusUnsupportedData, "bad cmd")
			return
		}
	}
	stream, resize, closeExec, err := s.RT.ExecTTY(ctx, r.URL.Query().Get("container"), cmd)
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "exec failed")
		return
	}
	defer closeExec()

	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if ws.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var m struct {
				Cols uint `json:"cols"`
				Rows uint `json:"rows"`
			}
			if json.Unmarshal(data, &m) == nil && m.Cols > 0 && m.Rows > 0 {
				_ = resize(m.Cols, m.Rows)
			}
			continue
		}
		if _, err := stream.Write(data); err != nil {
			return
		}
	}
}

// logs streams one container's marked log lines ("O " / "E " prefixes) as
// text frames, the same channel shape StreamLogsMarked returns locally.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()

	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	ch, stop, err := s.RT.StreamLogsMarked(ctx, r.URL.Query().Get("container"), tail)
	if err != nil {
		_ = ws.Close(websocket.StatusInternalError, "logs failed")
		return
	}
	defer stop()

	// A read that only ever errors is how this end notices the panel hung up;
	// nothing the panel sends on a log socket means anything.
	go func() {
		defer cancel()
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}()

	for line := range ch {
		if ws.Write(ctx, websocket.MessageText, []byte(line)) != nil {
			return
		}
	}
}

// --- volume move ----------------------------------------------------------

func (s *Server) moveReceive(w http.ResponseWriter, r *http.Request) {
	req, err := decode[MoveReceiveReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.RT.StartMoveReceiver(r.Context(), req.MoveID, req.Volume); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, struct{}{})
}

func (s *Server) moveClose(w http.ResponseWriter, r *http.Request) {
	req, err := decode[MoveReceiveReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.RT.StopMoveReceiver(r.Context(), req.MoveID); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, struct{}{})
}

// moveSend runs one rsync pass and streams rsync's own byte counters back as
// newline-delimited JSON, so the modal's progress bar and ETA are measured
// rather than guessed.
func (s *Server) moveSend(w http.ResponseWriter, r *http.Request) {
	req, err := decode[MoveSendReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)

	ctx := context.WithoutCancel(r.Context())
	err = s.RT.RunMoveSend(ctx, req.MoveID, req.Volume, req.Target, func(bytesDone, total int64) {
		_ = enc.Encode(MoveProgress{Bytes: bytesDone, Total: total})
		if flusher != nil {
			flusher.Flush()
		}
	})
	p := MoveProgress{Done: true}
	if err != nil {
		p.Error = err.Error()
	}
	_ = enc.Encode(p)
	if flusher != nil {
		flusher.Flush()
	}
}

// --- lifecycle ------------------------------------------------------------

// Run starts the agent listener and the sampler, and blocks until ctx ends.
//
// The listener binds the node's overlay address only: nothing on the host's
// LAN can reach it, which is the narrowing plan 30 step 7 asks for.
//
// Fails closed. net.JoinHostPort("", port) is ":port", every interface in the
// task's netns, so an address lookup that came back empty used to widen the
// agent instead of narrowing it, and the agent is root on its node by any
// other name.
func (s *Server) Run(ctx context.Context) error {
	cidr := os.Getenv("STACKR_OVERLAY_CIDR")
	addr := overlayAddr(cidr)
	if addr == "" {
		return fmt.Errorf("no address on the %s overlay (STACKR_OVERLAY_CIDR=%q); refusing to listen on every interface",
			runtime.NetworkName, cidr)
	}
	srv := &http.Server{
		Addr:    net.JoinHostPort(addr, strconv.Itoa(Port)),
		Handler: s.Handler(),
		// No write timeout: exec, logs and a volume tar all outlive any
		// sensible one. ReadHeaderTimeout still covers the slow-header case.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go s.sampleLoop(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("node agent listening", "addr", srv.Addr, "node", s.NodeID, "version", s.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// overlayAddr picks this task's address inside cidr, the stkr overlay's subnet
// as the panel inspected it (STACKR_OVERLAY_CIDR, set in the service spec).
//
// Matched on the prefix rather than taken as "the first private address":
// swarm gives a task one interface per attached network, and the task also
// holds docker_gwbridge, which is private too. Empty means the caller must not
// listen.
func overlayAddr(cidr string) string {
	if cidr == "" {
		return ""
	}
	_, prefix, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !prefix.Contains(ipn.IP) {
				continue
			}
			return ipn.IP.String()
		}
	}
	return ""
}

// sampleLoop posts this node's host metrics to the panel. It is a push, not a
// pull, so the panel needs no way in to a node it may not be able to reach.
func (s *Server) sampleLoop(ctx context.Context) {
	if s.PanelURL == "" {
		return
	}
	var prev hostmetrics.HostCounters
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-t.C:
			now := tick.UTC()
			cur := hostmetrics.ReadHost(now)
			hs, ok := cur.Sample(prev)
			prev = cur
			if !ok {
				continue // first tick only seeds the counters
			}
			if err := s.postSample(ctx, SampleReq{
				NodeID: s.NodeID, TS: now,
				CPUPct: hs.CPUPct, MemBytes: hs.MemBytes,
				RxBps: hs.RxBps, TxBps: hs.TxBps, DiskUsed: hs.DiskUsed,
			}); err != nil {
				slog.Debug("agent: posting sample", "error", err)
			}
		}
	}
}

func (s *Server) postSample(ctx context.Context, sample SampleReq) error {
	body, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(s.PanelURL, "/")+"/api/v1/nodes/samples", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set(VersionHeader, s.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("panel returned %s", resp.Status)
	}
	return nil
}
