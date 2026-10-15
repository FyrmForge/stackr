package worker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/wgapply"
)

// MeshSpec is POST /v1/mesh: everything the controller wants this node's mesh
// and exits to be, as one whole state. The node's private keys never travel;
// the worker makes them (wg.key, exit.key in the state dir) and GET /v1/mesh
// reports the public halves.
type MeshSpec struct {
	Iface      string // wg interface, mesh.Iface when empty
	Address    netip.Prefix
	ListenPort int
	Peers      []mesh.Peer
	// Links are the exits this node routes over, Serves the nodes this node is
	// an exit for. KeyFile and Uplink may be empty: the worker fills them.
	Links   []exits.Link
	Serves  []exits.Serve
	Ladders []LadderSpec
	// NoUplink are the env subnets of a node without egress: the worker drops
	// whatever of theirs would leave by the uplink (exits.Fence). Empty from
	// an old controller = no fence.
	NoUplink []string `json:",omitempty"`
	// Controller is the controller's mesh listener,
	// https://<controller transit>:<panel port>, where the node cert renews
	// over the tunnel. "" on the controller itself.
	Controller string `json:",omitempty"`
	// Panel is the panel's public URL, where the worker recovers its spec
	// when the controller goes quiet. "" on the controller itself.
	Panel string `json:",omitempty"`
	// TrustRoots is the cluster's trust list, one PEM root each: the current
	// root, and the previous one while a rollover runs. The worker trusts
	// exactly these (the worker owns applying it). Empty = the controller does
	// not send one (an old controller, or the controller's own spec): keep
	// the CA file from join.
	TrustRoots []string `json:",omitempty"`
	// ControllerSerials is the allow list of controller cert serials (hex):
	// the worker accepts a controller only when its cert's serial is listed.
	// Empty = no allow list, any controller cert from a trusted root.
	ControllerSerials []string `json:",omitempty"`
}

// LadderSpec is one subject (env subnets that share a ladder) and its rungs.
type LadderSpec struct {
	Subject exits.Subject
	Rungs   []exits.Rung
}

// MeshInfo is GET /v1/mesh.
type MeshInfo struct {
	WGPublic   string
	ExitPublic string
	Ladders    []LadderStatus
}

type LadderStatus struct {
	Sources []string
	State   exits.State
}

// MeshConfig is the node-side kernel boundary of the mesh.
type MeshConfig struct {
	WG            wgapply.Applier // wgapply.Kernel{} or a *wgapply.Recorder; nil = /v1/mesh answers 501
	Pinger        exits.Pinger    // nil = `ping -I iface -c1 -W2 target` through Config.Runner
	FailoverEvery time.Duration   // 0 = 15s
	LadderEvery   time.Duration   // 0 = 5s
	ReassertEvery time.Duration   // 0 = 30s; re-adds exit rules a chain rebuild flushed
	// WGPort is the UDP port this node's mesh listens on, reported in its
	// facts so the controller plans it; 0 = mesh.DefaultPort.
	WGPort int
}

type runningLadder struct {
	spec   LadderSpec
	l      *exits.Ladder
	cancel context.CancelFunc
	done   chan struct{}
}

type meshState struct {
	fo      *wgapply.Failover
	applied *MeshSpec // the last good apply, key files and uplink filled in
	ladders map[int]*runningLadder
	reMu    sync.Mutex
	// fence is the applied exits.Fence; zero = none.
	fence exits.Fence
	// legacyGone is set once the pre-stackr0 wg0 was checked for.
	legacyGone bool
}

// MeshFile in the state dir is the last applied mesh spec, replayed at
// start; the installer writes the first one from the join answer.
const MeshFile = "mesh.json"

func (s *Server) meshPath() string { return filepath.Join(s.cfg.StateDir, MeshFile) }

// loadKey reads a base64 WireGuard private key file, making it (0600) when
// missing, and returns it with its public key.
func loadKey(path string) (priv, pub string, err error) {
	b, err := os.ReadFile(path)
	var k wgtypes.Key
	switch {
	case err == nil:
		k, err = wgtypes.ParseKey(strings.TrimSpace(string(b)))
	case errors.Is(err, os.ErrNotExist):
		if k, err = wgtypes.GeneratePrivateKey(); err == nil {
			if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
				err = os.WriteFile(path, []byte(k.String()+"\n"), 0o600)
			}
		}
	}
	if err != nil {
		return "", "", fmt.Errorf("worker: key %s: %w", path, err)
	}
	return k.String(), k.PublicKey().String(), nil
}

// EnsureWGKey makes the node's WireGuard key in stateDir when missing and
// returns its public half: the installer needs it for the join request
// before any worker runs.
func EnsureWGKey(stateDir string) (string, error) {
	_, pub, err := loadKey(filepath.Join(stateDir, "wg.key"))
	return pub, err
}

func (s *Server) wgKeyPath() string   { return filepath.Join(s.cfg.StateDir, "wg.key") }
func (s *Server) exitKeyPath() string { return filepath.Join(s.cfg.StateDir, "exit.key") }

func (s *Server) exitRunner() exits.Runner {
	return exits.RunnerFunc(func(ctx context.Context, argv []string) error {
		out, err := s.cfg.Runner.Run(ctx, nil, argv[0], argv[1:]...)
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	})
}

type execPinger struct{ r Runner }

func (p execPinger) Ping(ctx context.Context, r exits.Rung) error {
	args := []string{"-c1", "-W2", r.Target}
	if r.Iface != "" {
		args = append([]string{"-I", r.Iface}, args...)
	}
	out, err := p.r.Run(ctx, nil, "ping", args...)
	if err != nil {
		return fmt.Errorf("ping %s: %w: %s", r.Target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Server) pinger() exits.Pinger {
	if s.cfg.Mesh.Pinger != nil {
		return s.cfg.Mesh.Pinger
	}
	return execPinger{s.cfg.Runner}
}

func dur(v, d time.Duration) time.Duration {
	if v <= 0 {
		return d
	}
	return v
}

// uplink is the interface of the default route, for an exit's MASQUERADE.
func (s *Server) uplink(ctx context.Context) (string, error) {
	out, err := s.cfg.Runner.Run(ctx, nil, "ip", "-o", "route", "get", "1.1.1.1")
	if err != nil {
		return "", fmt.Errorf("uplink: %w: %s", err, out)
	}
	f := strings.Fields(string(out))
	for i, w := range f {
		if w == "dev" && i+1 < len(f) {
			return f[i+1], nil
		}
	}
	return "", fmt.Errorf("uplink: no dev in %q", out)
}

// ApplyMesh makes the node match spec and saves it for replay. The ladders'
// first tick runs before it returns, so a caller that starts containers after
// it never sees a subject that is not routed yet. Safe to repeat: unchanged
// links and ladders are left alone. The controller's own node calls this in
// process.
func (s *Server) ApplyMesh(ctx context.Context, spec MeshSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.applyTrust(spec.TrustRoots, spec.ControllerSerials); err != nil {
		return err
	}
	if err := s.meshNet(spec); err != nil {
		return err
	}
	if err := s.meshExits(ctx, spec); err != nil {
		return err
	}
	b, _ := json.Marshal(spec)
	return writeAtomic(s.meshPath(), b)
}

// meshNet is the wg interface and the failover.
func (s *Server) meshNet(spec MeshSpec) error {
	if s.cfg.Mesh.WG == nil {
		return errNoWG
	}
	priv, _, err := loadKey(s.wgKeyPath())
	if err != nil {
		return err
	}
	iface := spec.Iface
	if iface == "" {
		iface = mesh.Iface
	}
	if !s.mesh.legacyGone {
		// Before stackr0 the mesh was wg0: drop it if it is ours, so the old
		// and new tunnels never fight over the peers. A user's own wg0 stays.
		if gone, err := s.cfg.Mesh.WG.RemoveOwned(mesh.LegacyIface, priv); err != nil {
			slog.Warn("worker: remove old wg0", "err", err)
		} else {
			s.mesh.legacyGone = true
			if gone {
				slog.Info("worker: removed the old wg0 mesh interface")
			}
		}
	}
	if err := s.cfg.Mesh.WG.Apply(wgapply.Config{Iface: iface, PrivateKey: priv,
		// The node's own port wins over a saved or pushed spec: a spec saved
		// before a port change would otherwise strand the node off the mesh.
		ListenPort: cmp.Or(s.cfg.Mesh.WGPort, mesh.DefaultPort), Address: spec.Address, Peers: spec.Peers}); err != nil {
		return err
	}
	if s.mesh.fo == nil {
		s.mesh.fo = &wgapply.Failover{A: s.cfg.Mesh.WG, Iface: iface}
		go s.mesh.fo.Run(s.ctx, dur(s.cfg.Mesh.FailoverEvery, 15*time.Second), func(err error) {
			slog.Warn("worker: wg failover", "err", err)
		})
	}
	s.mesh.fo.Reset(spec.Peers)
	return nil
}

// meshExits brings links, serves and ladders to spec. Only what changed since
// the last good apply is rebuilt (Up deletes and recreates the interface).
func (s *Server) meshExits(ctx context.Context, spec MeshSpec) error {
	run := s.exitRunner()
	if len(spec.Links)+len(spec.Serves)+len(spec.Ladders) > 0 {
		if _, _, err := loadKey(s.exitKeyPath()); err != nil {
			return err
		}
	}
	var uplink string
	links := append([]exits.Link(nil), spec.Links...)
	for i := range links {
		if links[i].KeyFile == "" {
			links[i].KeyFile = s.exitKeyPath()
		}
	}
	serves := append([]exits.Serve(nil), spec.Serves...)
	for i := range serves {
		if serves[i].KeyFile == "" {
			serves[i].KeyFile = s.exitKeyPath()
		}
		if serves[i].Uplink == "" {
			if uplink == "" {
				var err error
				if uplink, err = s.uplink(ctx); err != nil {
					return err
				}
			}
			serves[i].Uplink = uplink
		}
	}
	prev := s.mesh.applied
	if prev == nil {
		prev = &MeshSpec{}
	}

	// Stop what changed or went and clear its rules before links move.
	want := map[int]LadderSpec{}
	for _, l := range spec.Ladders {
		want[l.Subject.PrefBase] = l
	}
	for base, rl := range s.mesh.ladders {
		if w, ok := want[base]; ok && reflect.DeepEqual(w, rl.spec) {
			continue
		}
		rl.cancel()
		<-rl.done
		if _, ok := want[base]; !ok {
			if err := exits.Apply(ctx, run, rl.spec.Subject.Clear()); err != nil {
				return err
			}
		}
		delete(s.mesh.ladders, base)
	}

	oldL, oldS := map[string]exits.Link{}, map[string]exits.Serve{}
	for _, l := range prev.Links {
		oldL[l.Iface] = l
	}
	for _, v := range prev.Serves {
		oldS[v.Iface] = v
	}
	for _, l := range links {
		o, ok := oldL[l.Iface]
		delete(oldL, l.Iface)
		if ok && reflect.DeepEqual(o, l) {
			continue
		}
		steps, err := l.Up()
		if err == nil {
			err = exits.Apply(ctx, run, steps)
		}
		if err != nil {
			return err
		}
	}
	for _, l := range oldL {
		_ = exits.Apply(ctx, run, l.Down())
	}
	for _, v := range serves {
		o, ok := oldS[v.Iface]
		delete(oldS, v.Iface)
		if ok && reflect.DeepEqual(o, v) {
			continue
		}
		steps, err := v.Up()
		if err == nil {
			err = exits.Apply(ctx, run, steps)
		}
		if err != nil {
			return err
		}
	}
	for _, v := range oldS {
		_ = exits.Apply(ctx, run, v.Down())
	}

	// The first tick of every new ladder runs here, before the caller starts
	// containers on this node.
	if s.mesh.ladders == nil {
		s.mesh.ladders = map[int]*runningLadder{}
	}
	for _, w := range spec.Ladders {
		if s.mesh.ladders[w.Subject.PrefBase] != nil {
			continue
		}
		l := exits.NewLadder(w.Subject, w.Rungs, run, s.pinger())
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := l.Tick(tctx)
		cancel()
		if err != nil {
			return fmt.Errorf("ladder %v: %w", w.Subject.Sources, err)
		}
		lctx, lcancel := context.WithCancel(s.ctx)
		rl := &runningLadder{spec: w, l: l, cancel: lcancel, done: make(chan struct{})}
		go func() {
			defer close(rl.done)
			l.Loop(lctx, dur(s.cfg.Mesh.LadderEvery, 5*time.Second), func(err error) {
				slog.Warn("worker: exit ladder", "sources", w.Subject.Sources, "err", err)
			})
		}()
		s.mesh.ladders[w.Subject.PrefBase] = rl
	}
	if err := s.meshFence(ctx, run, spec.NoUplink); err != nil {
		return err
	}
	cp := spec
	cp.Links, cp.Serves = links, serves
	s.mesh.applied = &cp
	return nil
}

// meshFence brings the uplink fence to the wanted env subnets. It runs after
// the ladders, so the blackhole is up before the fence is.
func (s *Server) meshFence(ctx context.Context, run exits.Runner, noUplink []string) error {
	var want exits.Fence
	if len(noUplink) > 0 {
		up, err := s.uplink(ctx)
		if err != nil {
			return err
		}
		want = exits.Fence{Sources: slices.Sorted(slices.Values(noUplink)), Uplink: up}
	}
	if reflect.DeepEqual(want, s.mesh.fence) {
		return nil
	}
	if len(s.mesh.fence.Sources) > 0 {
		_ = exits.Apply(ctx, run, s.mesh.fence.Down())
	}
	s.mesh.fence = exits.Fence{}
	if len(want.Sources) > 0 {
		steps, err := want.Up()
		if err == nil {
			err = exits.Apply(ctx, run, steps)
		}
		if err != nil {
			return err
		}
		s.mesh.fence = want
	}
	return nil
}

// reassert re-adds the iptables rules of every applied link and serve. The
// controller's chain rebuild redeclares STACKR-NAT, STACKR-RAW and
// STACKR-XN, which empties them; the steps that carry a check are exactly the
// idempotent rule adds, so the links themselves are not touched.
func (s *Server) reassert(ctx context.Context) error {
	s.mesh.reMu.Lock()
	defer s.mesh.reMu.Unlock()
	a := s.mesh.applied
	if a == nil {
		return nil
	}
	var steps []exits.Step
	for _, l := range a.Links {
		up, err := l.Up()
		if err != nil {
			return err
		}
		steps = append(steps, checked(up)...)
	}
	for _, v := range a.Serves {
		up, err := v.Up()
		if err != nil {
			return err
		}
		steps = append(steps, checked(up)...)
	}
	if len(s.mesh.fence.Sources) > 0 {
		up, err := s.mesh.fence.Up()
		if err != nil {
			return err
		}
		steps = append(steps, checked(up)...)
	}
	return exits.Apply(ctx, s.exitRunner(), steps)
}

func checked(in []exits.Step) []exits.Step {
	var out []exits.Step
	for _, st := range in {
		if st.Check != nil {
			out = append(out, st)
		}
	}
	return out
}

// Reassert is for an in-process caller that rebuilds the chains itself (the
// controller's own node), right after the rebuild.
func (s *Server) Reassert(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reassert(ctx)
}

func (s *Server) reassertLoop() {
	t := time.NewTicker(dur(s.cfg.Mesh.ReassertEvery, 30*time.Second))
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			if err := s.Reassert(s.ctx); err != nil {
				slog.Warn("worker: reassert exit rules", "err", err)
			}
			s.mu.Lock()
			err := s.jumpsFirst(s.ctx, s.savedJumps())
			s.mu.Unlock()
			if err != nil {
				slog.Warn("worker: iptables jumps", "err", err)
			}
		}
	}
}

// MeshInfo is the node's public keys and the state of each ladder.
func (s *Server) MeshInfo() (MeshInfo, error) {
	_, wgPub, err := loadKey(s.wgKeyPath())
	if err != nil {
		return MeshInfo{}, err
	}
	_, exPub, err := loadKey(s.exitKeyPath())
	if err != nil {
		return MeshInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info := MeshInfo{WGPublic: wgPub, ExitPublic: exPub}
	for _, rl := range s.mesh.ladders {
		info.Ladders = append(info.Ladders, LadderStatus{Sources: rl.spec.Subject.Sources, State: rl.l.State()})
	}
	return info, nil
}

func (s *Server) postMesh(w http.ResponseWriter, r *http.Request) {
	b, ok := readBody(w, r)
	if !ok {
		return
	}
	var spec MeshSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.ApplyMesh(r.Context(), spec); err != nil {
		code := 500
		if errors.Is(err, errNoWG) {
			code = 501
		}
		fail(w, code, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) getMesh(w http.ResponseWriter, r *http.Request) {
	info, err := s.MeshInfo()
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

// savedMesh is the last applied mesh spec, nil when none was applied.
func (s *Server) savedMesh() (*MeshSpec, error) {
	b, err := os.ReadFile(s.meshPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var spec MeshSpec
	if err == nil {
		err = json.Unmarshal(b, &spec)
	}
	if err != nil {
		return nil, err
	}
	return &spec, nil
}

// Stop ends the background loops (failover, ladders, reassert).
func (s *Server) Stop() {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rl := range s.mesh.ladders {
		rl.cancel()
		<-rl.done
	}
}
