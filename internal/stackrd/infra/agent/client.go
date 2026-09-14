package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

// Client is the panel's side of one node's agent. It carries the node's
// overlay address, which the panel reads out of swarm task state, there is
// nothing to configure and nothing to discover.
type Client struct {
	// Addr is the agent task's address on the stkr overlay.
	Addr string
	Key  string
	// Version is the panel's own version, sent on every call. HTTP has no
	// "connect", so this is where the handshake lives: the agent echoes its
	// version back and Do refuses a mismatch.
	Version string

	HTTP *http.Client
}

// ErrVersionMismatch is returned when the agent on a node is not the same
// build as the panel. The node shows as "agent outdated": the fix is to
// update the agent service, which is one service update for the whole swarm.
type ErrVersionMismatch struct {
	Panel, Agent string
}

func (e ErrVersionMismatch) Error() string {
	return fmt.Sprintf("node agent is version %q, panel is %q; update the agent service", e.Agent, e.Panel)
}

// sharedClient is the default transport for every agent call. One, not one
// per request: a fresh http.Transport pools nothing, so every call to every
// node opened a new TCP connection and left it to idle out, and the metrics
// poll alone makes one per node per tick.
var sharedClient = &http.Client{Transport: &http.Transport{
	// No overall timeout: exec, logs, a volume tar and an rsync pass all
	// outlive any sensible one. The dial and the response headers are where a
	// dead node has to be caught, and both are bounded here.
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	ResponseHeaderTimeout: 30 * time.Second,
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       90 * time.Second,
}}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return sharedClient
}

func (c *Client) base() string {
	return "http://" + net.JoinHostPort(c.Addr, strconv.Itoa(Port))
}

func (c *Client) request(ctx context.Context, method, path string, q url.Values, body io.Reader) (*http.Response, error) {
	u := c.base() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set(VersionHeader, c.Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if got := resp.Header.Get(VersionHeader); got != "" && c.Version != "" && got != c.Version {
		_ = resp.Body.Close()
		return nil, ErrVersionMismatch{Panel: c.Version, Agent: got}
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		var e ErrResp
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return nil, fmt.Errorf("agent %s: %s", c.Addr, e.Error)
	}
	return resp, nil
}

// call is one JSON in, JSON out request.
func call[Req any, Resp any](ctx context.Context, c *Client, path string, in Req) (Resp, error) {
	var out Resp
	body, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	resp, err := c.request(ctx, http.MethodPost, path, nil, strings.NewReader(string(body)))
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && err != io.EOF {
		return out, err
	}
	return out, nil
}

// --- the runtime methods, remote --------------------------------------------
//
// Every method below mirrors the signature of the infra/runtime method of the
// same name, so node.go can pick socket or agent without either caller or
// callee knowing which it got.

func (c *Client) Info(ctx context.Context) (InfoResp, error) {
	return call[struct{}, InfoResp](ctx, c, PathInfo, struct{}{})
}

func (c *Client) InspectContainer(ctx context.Context, id string) (*runtime.ContainerDetail, error) {
	return call[ContainerReq, *runtime.ContainerDetail](ctx, c, PathInspect, ContainerReq{Container: id})
}

func (c *Client) Stats(ctx context.Context, id string) (runtime.ContainerStats, error) {
	return call[ContainerReq, runtime.ContainerStats](ctx, c, PathStats, ContainerReq{Container: id})
}

func (c *Client) HealthStatus(ctx context.Context, id string) (string, error) {
	out, err := call[ContainerReq, map[string]string](ctx, c, PathHealth, ContainerReq{Container: id})
	return out["status"], err
}

func (c *Client) PauseContainer(ctx context.Context, id string) error {
	_, err := call[PauseReq, struct{}](ctx, c, PathPause, PauseReq{Container: id})
	return err
}

func (c *Client) UnpauseContainer(ctx context.Context, id string) error {
	_, err := call[PauseReq, struct{}](ctx, c, PathPause, PauseReq{Container: id, Unpause: true})
	return err
}

func (c *Client) Prune(ctx context.Context) (string, error) {
	out, err := call[struct{}, PruneResp](ctx, c, PathPrune, struct{}{})
	return out.Output, err
}

func (c *Client) ListAll(ctx context.Context) ([]runtime.ManagedContainer, error) {
	return call[struct{}, []runtime.ManagedContainer](ctx, c, PathContainers, struct{}{})
}

func (c *Client) containerAction(ctx context.Context, id, action string) error {
	_, err := call[ContainerActionReq, struct{}](ctx, c, PathContainerAction,
		ContainerActionReq{Container: id, Action: action})
	return err
}

func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.containerAction(ctx, id, "start")
}

func (c *Client) StopContainer(ctx context.Context, id string) error {
	return c.containerAction(ctx, id, "stop")
}

func (c *Client) StopRemove(ctx context.Context, id string) error {
	return c.containerAction(ctx, id, "remove")
}

func (c *Client) ContainerIsSystem(ctx context.Context, id string) bool {
	out, err := call[ContainerReq, BoolResp](ctx, c, PathContainerSystem, ContainerReq{Container: id})
	return err == nil && out.OK
}

func (c *Client) ListVolumes(ctx context.Context) ([]runtime.VolumeInfo, error) {
	return call[struct{}, []runtime.VolumeInfo](ctx, c, PathVolumes, struct{}{})
}

func (c *Client) ListVolumeFiles(ctx context.Context, vol, dir string) ([]runtime.VolumeEntry, error) {
	return call[VolumeReq, []runtime.VolumeEntry](ctx, c, PathVolumeList, VolumeReq{Volume: vol, Dir: dir})
}

func (c *Client) DeleteVolumeFile(ctx context.Context, vol, file string) error {
	_, err := call[VolumeReq, struct{}](ctx, c, PathVolumeDelete, VolumeReq{Volume: vol, File: file})
	return err
}

// CreateVolume makes a named volume on the node this client points at.
func (c *Client) CreateVolume(ctx context.Context, vol string) error {
	_, err := call[VolumeReq, struct{}](ctx, c, PathVolumeCreate, VolumeReq{Volume: vol})
	return err
}

// CreateVolumeOpts makes a volume with an explicit driver and driver opts on
// the node this client points at, how a storage sub-path gets its nfs/cifs
// mount on the machine that will serve it.
func (c *Client) CreateVolumeOpts(ctx context.Context, vol, driver string, opts map[string]string) error {
	_, err := call[VolumeReq, struct{}](ctx, c, PathVolumeCreate,
		VolumeReq{Volume: vol, Driver: driver, Opts: opts})
	return err
}

// RemoveVolume destroys a named volume on the node this client points at.
// Not to be confused with DeleteVolumeFile, which removes one file inside one.
func (c *Client) RemoveVolume(ctx context.Context, vol string) error {
	_, err := call[VolumeReq, struct{}](ctx, c, PathVolumeRemove, VolumeReq{Volume: vol})
	return err
}

func (c *Client) VolumeSize(ctx context.Context, vol string) (int64, error) {
	out, err := call[VolumeReq, SizeResp](ctx, c, PathVolumeSize, VolumeReq{Volume: vol})
	return out.Bytes, err
}

// ReadVolumeFile streams one file off the node. The wait func is where a
// mid-stream failure surfaces, matching the local method: by the time the
// body starts there is no status code left to say it with. It used to only
// close the body, so every mid-stream failure read as success.
func (c *Client) ReadVolumeFile(ctx context.Context, vol, file string) (io.ReadCloser, func() error, error) {
	resp, err := c.request(ctx, http.MethodGet, PathVolumeRead,
		url.Values{"volume": {vol}, "file": {file}}, nil)
	if err != nil {
		return nil, nil, err
	}
	return resp.Body, func() error { return c.streamWait(resp) }, nil
}

// streamWait drains a stream to its end and turns the trailer into an error.
// Draining first is the point: a trailer only exists once the body is fully
// read, so a caller that stopped early would read an empty one and call a
// failed transfer a success.
func (c *Client) streamWait(resp *http.Response) error {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if msg := resp.Trailer.Get(StreamErrorTrailer); msg != "" {
		return fmt.Errorf("agent %s: %s", c.Addr, msg)
	}
	return nil
}

func (c *Client) WriteVolumeFile(ctx context.Context, vol, file string, src io.Reader) error {
	resp, err := c.request(ctx, http.MethodPost, PathVolumeWrite,
		url.Values{"volume": {vol}, "file": {file}}, src)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *Client) TarVolume(ctx context.Context, vol string, w io.Writer, live bool) error {
	q := url.Values{"volume": {vol}}
	if live {
		q.Set("live", "1")
	}
	resp, err := c.request(ctx, http.MethodGet, PathVolumeTar, q, nil)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		_ = resp.Body.Close()
		return err
	}
	// The tar ending is not the tar being whole: the trailer is what says the
	// agent finished writing it, and a green backup holding half an archive is
	// worse than no backup at all.
	return c.streamWait(resp)
}

func (c *Client) UntarVolume(ctx context.Context, vol string, src io.Reader) error {
	resp, err := c.request(ctx, http.MethodPost, PathVolumeUntar, url.Values{"volume": {vol}}, src)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// ExecStream runs one non-TTY exec on the node, mirroring the local method:
// stdin goes up as the request body, stdout comes back as the response body,
// and the command's own failure arrives in a trailer that the wait func
// reads. Reading the body to EOF before wait() is what makes the trailer
// available, which is the same order the local caller already uses.
func (c *Client) ExecStream(ctx context.Context, id string, cmd []string, stdin io.Reader) (io.Reader, func() error, error) {
	q := url.Values{"container": {id}}
	if len(cmd) > 0 {
		b, err := json.Marshal(cmd)
		if err != nil {
			return nil, nil, err
		}
		q.Set("cmd", string(b))
	}
	resp, err := c.request(ctx, http.MethodPost, PathExecRun, q, stdin)
	if err != nil {
		return nil, nil, err
	}
	wait := func() error {
		// Drain first: a caller that stopped early would otherwise read a
		// trailer that has not arrived and call a failed command a success.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if msg := resp.Trailer.Get(ExecErrorTrailer); msg != "" {
			return fmt.Errorf("agent %s: %s", c.Addr, msg)
		}
		return nil
	}
	return resp.Body, wait, nil
}

func (c *Client) ExecShell(ctx context.Context, id, command string) (string, error) {
	out, err := call[ExecShellReq, ExecShellResp](ctx, c, PathExecShell,
		ExecShellReq{Container: id, Command: command})
	if err != nil {
		return out.Output, err
	}
	if out.Error != "" {
		return out.Output, fmt.Errorf("%s", out.Error)
	}
	return out.Output, nil
}

// --- streams --------------------------------------------------------------

// ExecTTY opens a TTY exec on the node and returns it in the same shape the
// local method does, so handlers/web/wsterm bridges a browser to a remote
// container without knowing it is remote.
func (c *Client) ExecTTY(ctx context.Context, id string, cmd []string) (io.ReadWriter, func(w, h uint) error, func(), error) {
	q := url.Values{"container": {id}}
	if len(cmd) > 0 {
		b, err := json.Marshal(cmd)
		if err != nil {
			return nil, nil, nil, err
		}
		q.Set("cmd", string(b))
	}
	ws, err := c.dial(ctx, PathExec, q)
	if err != nil {
		return nil, nil, nil, err
	}
	stream := &wsStream{ws: ws, ctx: ctx}
	resize := func(w, h uint) error {
		b, err := json.Marshal(map[string]any{"type": "resize", "cols": w, "rows": h})
		if err != nil {
			return err
		}
		return ws.Write(ctx, websocket.MessageText, b)
	}
	return stream, resize, func() { _ = ws.CloseNow() }, nil
}

// StreamLogsMarked follows a container's logs on the node, one "O "/"E "
// prefixed line per message, the same channel the local method returns.
func (c *Client) StreamLogsMarked(ctx context.Context, id string, tail int) (<-chan string, func(), error) {
	ws, err := c.dial(ctx, PathLogs, url.Values{
		"container": {id}, "tail": {strconv.Itoa(tail)},
	})
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan string, 256)
	go func() {
		defer close(ch)
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			select {
			case ch <- string(data):
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, func() { _ = ws.CloseNow() }, nil
}

func (c *Client) dial(ctx context.Context, path string, q url.Values) (*websocket.Conn, error) {
	u := "ws://" + net.JoinHostPort(c.Addr, strconv.Itoa(Port)) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	ws, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPClient: c.httpClient(),
		HTTPHeader: http.Header{
			"Authorization": {"Bearer " + c.Key},
			VersionHeader:   {c.Version},
		},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	if resp != nil {
		if got := resp.Header.Get(VersionHeader); got != "" && c.Version != "" && got != c.Version {
			_ = ws.CloseNow()
			return nil, ErrVersionMismatch{Panel: c.Version, Agent: got}
		}
	}
	// The default read limit is 32KB, which a single burst of terminal
	// output or one log line can pass. Streams are framed, not bounded.
	ws.SetReadLimit(-1)
	return ws, nil
}

// wsStream adapts a websocket to io.ReadWriter so an exec looks the same
// whether it came off the local socket or off a node.
type wsStream struct {
	ws  *websocket.Conn
	ctx context.Context
	buf []byte
}

func (s *wsStream) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		typ, data, err := s.ws.Read(s.ctx)
		if err != nil {
			return 0, err
		}
		if typ != websocket.MessageBinary {
			continue
		}
		s.buf = data
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *wsStream) Write(p []byte) (int, error) {
	if err := s.ws.Write(s.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// --- volume move ----------------------------------------------------------

// OpenMoveReceiver asks this node to accept a volume move into vol.
func (c *Client) OpenMoveReceiver(ctx context.Context, moveID, vol string) error {
	_, err := call[MoveReceiveReq, struct{}](ctx, c, PathMoveReceive,
		MoveReceiveReq{MoveID: moveID, Volume: vol})
	return err
}

// CloseMoveReceiver tears the receiving container down. The volume it wrote
// stays, it is the point of the move.
func (c *Client) CloseMoveReceiver(ctx context.Context, moveID string) error {
	_, err := call[MoveReceiveReq, struct{}](ctx, c, PathMoveClose,
		MoveReceiveReq{MoveID: moveID})
	return err
}

// SendMove runs one rsync pass from this node into the receiver, reporting
// rsync's own byte counters as they arrive. It returns when the pass ends.
func (c *Client) SendMove(ctx context.Context, moveID, vol, target string, progress func(done, total int64)) error {
	body, err := json.Marshal(MoveSendReq{MoveID: moveID, Volume: vol, Target: target})
	if err != nil {
		return err
	}
	resp, err := c.request(ctx, http.MethodPost, PathMoveSend, nil, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var p MoveProgress
		if json.Unmarshal(sc.Bytes(), &p) != nil {
			continue
		}
		if p.Done {
			if p.Error != "" {
				return fmt.Errorf("%s", p.Error)
			}
			return nil
		}
		if progress != nil {
			progress(p.Bytes, p.Total)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// The stream ended without a done frame: the agent died mid-pass. Not a
	// success, a move that reports one would let the caller delete the
	// source volume.
	return fmt.Errorf("agent %s: move stream ended without finishing", c.Addr)
}
