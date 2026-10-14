package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/move"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Moving an env between nodes: flow/move does the phases, this file gives it
// its hands. Flows may not call flows, so deploy is reached through moveEnv,
// built here from the leaves and the orchestrator's own deploy and proxy sync.

// EnvMove is a row of env_moves.
type EnvMove = store.EnvMove

type moveJob struct {
	MoveID string `json:"move_id"`
	EnvID  string `json:"env_id"`
	By     string `json:"by,omitempty"`
	KeyID  string `json:"key_id,omitempty"`
}

// moveFlow is the flow wired to this orchestrator; it holds no state of its
// own (the rows are the state), so it is cheap to build per call.
func (o *Orchestrator) moveFlow() *move.Flow {
	return &move.Flow{Moves: o.store.Moves, Nodes: moveNodes{o}, Env: moveEnv{o}, EnsureRsync: o.helperReady}
}

// rsyncDockerfile is the live-move helper. The controller builds it once
// (its own internet only) and pushes it to the cluster registry; nodes pull
// it from there over the mesh.
const rsyncDockerfile = "FROM alpine:3\nRUN apk add --no-cache rsync\n"

// rsyncSteps puts move.RsyncImage in the registry: there already = done,
// else push the controller's local copy, building it first if missing.
type rsyncSteps struct {
	inRegistry func(context.Context) (bool, error)
	local      func(context.Context) bool
	build      func(context.Context) error
	push       func(context.Context) error
}

func (s rsyncSteps) ensure(ctx context.Context) error {
	if ok, err := s.inRegistry(ctx); err != nil || ok {
		return err
	}
	if !s.local(ctx) {
		if err := s.build(ctx); err != nil {
			return fmt.Errorf("build: %w", err)
		}
	}
	return s.push(ctx)
}

func (o *Orchestrator) rsyncSteps() rsyncSteps {
	ref := move.RsyncImage
	repo, tag, _ := strings.Cut(strings.TrimPrefix(ref, "localhost:"+installspec.RegistryPort+"/"), ":")
	return rsyncSteps{
		inRegistry: func(ctx context.Context) (bool, error) {
			url := "http://127.0.0.1:" + installspec.RegistryPort + "/v2/" + repo + "/manifests/" + tag
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
			if err != nil {
				return false, err
			}
			req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return false, fmt.Errorf("registry: %w", err)
			}
			_ = res.Body.Close()
			return res.StatusCode == http.StatusOK, nil
		},
		local: func(ctx context.Context) bool {
			ctx, err := o.withController(ctx)
			if err != nil {
				return false
			}
			_, err = o.docker.LocalDigest(ctx, ref)
			return err == nil
		},
		build: func(ctx context.Context) error {
			dir, err := os.MkdirTemp(o.cfg.DataDir, "rsync-build-")
			if err != nil {
				return err
			}
			defer func() { _ = os.RemoveAll(dir) }()
			df := filepath.Join(dir, "Dockerfile")
			if err := os.WriteFile(df, []byte(rsyncDockerfile), 0o644); err != nil {
				return err
			}
			ctx, err = o.withController(ctx)
			if err != nil {
				return err
			}
			_, err = o.images.Build(ctx, dir, df, ref, nil, io.Discard)
			return err
		},
		push: func(ctx context.Context) error {
			leave, err := o.regs.Hold(ctx)
			if err != nil {
				return err
			}
			defer leave()
			if err := pushLocal(ctx, ref); err != nil {
				return err
			}
			// the push went past the front, so write the system row here
			o.registryPushed(ctx, regfront.Pushed{Repo: repo, Reference: tag, Kind: regfront.KindNode})
			return nil
		},
	}
}

// pushLocal pushes a localhost:5000/... image from the controller's daemon
// into the cluster registry (on the controller that address is the registry).
func pushLocal(ctx context.Context, ref string) error {
	// ponytail: the docker CLI, as image builds use; the client has no push.
	if out, err := exec.CommandContext(ctx, "docker", "push", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("push %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// MoveEnv (org owner; the target must be open to the org) moves an env to
// another node as a job: cold (stop, copy, start) or live (rsync while it
// runs, then stop and a checksum pass).
func (o *Orchestrator) MoveEnv(ctx context.Context, by *Principal, envID, toNode string, live bool) (Job, error) {
	if by == nil {
		return Job{}, errs.ErrRefused
	}
	orgID, err := o.placementOrg(ctx, ScopeEnv, envID)
	if err != nil {
		return Job{}, err
	}
	if err := authz.Can(by.Access, "placement.write", authz.Resource{OrgID: orgID}); err != nil {
		return Job{}, err
	}
	from, err := o.nodeOfEnvID(ctx, envID)
	if err != nil {
		return Job{}, err
	}
	to, err := o.nodeByRef(ctx, toNode)
	if err != nil {
		return Job{}, err
	}
	if err := o.nodeUsable(ctx, by, to, orgID); err != nil {
		return Job{}, err
	}
	m, err := o.moveFlow().Begin(ctx, move.Request{EnvID: envID, From: from.ID, To: to.ID, Live: live})
	if err != nil {
		return Job{}, err
	}
	return o.enqueueMove(ctx, m, by.User.ID, by.KeyID)
}

// enqueueMove locks the env and every tile it has, like a promote.
func (o *Orchestrator) enqueueMove(ctx context.Context, m EnvMove, by, keyID string) (Job, error) {
	ts, err := o.tiles.List(ctx, m.EnvID)
	if err != nil {
		return Job{}, err
	}
	lock := []string{"env:" + m.EnvID}
	for _, t := range ts {
		lock = append(lock, t.ID)
	}
	return o.enqueue(ctx, kindMoveEnv, moveJob{MoveID: m.ID, EnvID: m.EnvID, By: by, KeyID: keyID}, lock...)
}

// resumeMoves queues a job for every move a restart cut off.
func (o *Orchestrator) resumeMoves(ctx context.Context) error {
	ms, err := o.moveFlow().Unfinished(ctx)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if _, err := o.enqueueMove(ctx, m, "", ""); err != nil {
			return fmt.Errorf("resume move %s: %w", m.ID, err)
		}
	}
	return nil
}

// runMove is the "move-env" job. A live move admits the source node to the
// target's rsync ports for as long as it runs.
func (o *Orchestrator) runMove(ctx context.Context, r *jobs.Run, p moveJob) error {
	ctx, err := o.withController(ctx)
	if err != nil {
		return err
	}
	f := o.moveFlow()
	m, err := o.store.Moves.Get(ctx, p.MoveID)
	if err != nil {
		return err
	}
	if m.Phase == move.Queued {
		o.auditMove(ctx, securitylog.MoveStart, p, m, "")
	}
	if m.Live {
		closeFW, err := o.openRsync(ctx, m)
		if err != nil {
			return err
		}
		defer closeFW()
	}
	m, err = f.Run(context.WithValue(ctx, moveLogKey{}, r.Log), p.MoveID, r.Log)
	if m.Phase == move.Done || m.Phase == move.RolledBack {
		cause := ""
		if err != nil {
			cause = err.Error()
		}
		o.auditMove(ctx, securitylog.MoveFinish, p, m, cause)
	}
	if err == nil && m.Phase == move.Done {
		err = o.relinkMoved(ctx, m.EnvID, m.FromNode, m.ToNode)
	}
	return err
}

func (o *Orchestrator) auditMove(ctx context.Context, action string, p moveJob, m EnvMove, cause string) {
	detail := fmt.Sprintf("%s -> %s live=%t phase=%s", m.FromNode, m.ToNode, m.Live, m.Phase)
	if cause != "" {
		detail += " error=" + cause
	}
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		return audit(tx).Append(ctx, securitylog.Event{
			Action: action, ActorID: p.By, KeyID: p.KeyID,
			TargetKind: "env", TargetID: m.EnvID, Detail: detail,
		})
	})
	if err != nil {
		slog.Warn("move: audit row not written", "move", m.ID, "err", err)
	}
}

// portAllower is a node's firewall table (vip.Table; a worker's remote one).
type portAllower interface {
	AllowTCP(ctx context.Context, src string, lo, hi int, on bool) error
}

// openRsync admits the source node's address to the target's rsync ports and
// returns the undo. A table without a firewall (a test stub) admits nothing.
func (o *Orchestrator) openRsync(ctx context.Context, m EnvMove) (func(), error) {
	vols, err := moveEnv{o}.Volumes(ctx, m.EnvID)
	if err != nil {
		return nil, err
	}
	src, err := moveNodes{o}.Addr(ctx, m.FromNode)
	if err != nil {
		return nil, err
	}
	tctx := docker.WithNode(ctx, m.ToNode)
	t, err := o.vipFor(tctx)
	if err != nil {
		return nil, err
	}
	fw, ok := t.(portAllower)
	if !ok || len(vols) == 0 {
		return func() {}, nil
	}
	hi := move.RsyncPort + len(vols) - 1
	if err := fw.AllowTCP(tctx, src, move.RsyncPort, hi, true); err != nil {
		return nil, fmt.Errorf("open rsync ports on the target: %w", err)
	}
	return func() {
		if err := fw.AllowTCP(context.WithoutCancel(tctx), src, move.RsyncPort, hi, false); err != nil {
			slog.Warn("move: rsync ports left open on the target", "node", m.ToNode, "err", err)
		}
	}, nil
}

// moveNodes is move.Nodes over the router.
type moveNodes struct{ o *Orchestrator }

func (n moveNodes) Docker(nodeID string) (move.Docker, error) {
	return nodeDocker{n.o.docker, nodeID}, nil
}

// Addr is the node's WireGuard address, without a mask.
func (n moveNodes) Addr(ctx context.Context, nodeID string) (string, error) {
	nd, err := n.o.store.Nodes.Get(ctx, nodeID)
	if err != nil {
		return "", err
	}
	if t := transitOf(nd); t.IsValid() {
		return t.String(), nil
	}
	return "", errs.Conflictf("node %s has no WireGuard address", nd.Name)
}

// nodeDocker is one node's slice of the router: every call carries the node.
type nodeDocker struct {
	d  Docker
	id string
}

func (n nodeDocker) c(ctx context.Context) context.Context { return docker.WithNode(ctx, n.id) }

func (n nodeDocker) CreateVolume(ctx context.Context, name, driver string, opts, labels map[string]string) error {
	return n.d.CreateVolume(n.c(ctx), name, driver, opts, labels)
}
func (n nodeDocker) InspectVolume(ctx context.Context, name string) (docker.VolumeInfo, error) {
	return n.d.InspectVolume(n.c(ctx), name)
}
func (n nodeDocker) EnsureTool(ctx context.Context) error { return n.d.EnsureTool(n.c(ctx)) }
func (n nodeDocker) TarVolume(ctx context.Context, name string, w io.Writer, live bool) error {
	return n.d.TarVolume(n.c(ctx), name, w, live)
}
func (n nodeDocker) UntarVolume(ctx context.Context, name string, src io.Reader) error {
	return n.d.UntarVolume(n.c(ctx), name, src)
}
func (n nodeDocker) Pull(ctx context.Context, ref, auth string, w io.Writer) error {
	return n.d.Pull(n.c(ctx), ref, auth, w)
}
func (n nodeDocker) LocalDigest(ctx context.Context, ref string) (string, error) {
	return n.d.LocalDigest(n.c(ctx), ref)
}
func (n nodeDocker) Run(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	return n.d.Run(n.c(ctx), spec)
}
func (n nodeDocker) Wait(ctx context.Context, id string) (int, error) { return n.d.Wait(n.c(ctx), id) }
func (n nodeDocker) StopRemove(ctx context.Context, id string) error {
	return n.d.StopRemove(n.c(ctx), id)
}

// moveLogKey carries the job's log to the env adapter (move.Env has no log).
type moveLogKey struct{}

func moveLog(ctx context.Context) io.Writer {
	if w, ok := ctx.Value(moveLogKey{}).(io.Writer); ok {
		return w
	}
	return io.Discard
}

// moveEnv is move.Env over the tile and environment leaves and flow/deploy.
type moveEnv struct{ o *Orchestrator }

func (e moveEnv) Volumes(ctx context.Context, envID string) ([]store.Volume, error) {
	vs, err := e.o.volumes.List(ctx, volume.Scope{Kind: "env", ID: envID})
	return slices.DeleteFunc(vs, func(v store.Volume) bool { return v.OrphanedAt != nil }), err
}

// Stop stops every replica of the env on the node it is on now.
func (e moveEnv) Stop(ctx context.Context, envID string) error {
	ts, err := e.o.tiles.List(ctx, envID)
	if err != nil {
		return err
	}
	for _, t := range ts {
		cs, err := e.o.tiles.Replicas(ctx, t)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.State != "running" {
				continue
			}
			if err := e.o.tiles.Stop(ctx, t.ID, c.ID); err != nil {
				return fmt.Errorf("stop %s: %w", t.Slug, err)
			}
		}
	}
	return nil
}

// Start makes nodeID the env's node (a fresh /24 from its range) and deploys
// every tile there, dependencies first, then re-publishes the proxy config
// and rebuilds every node's VIP table, which rewires links, ingress routes
// and exits. A repeat redeploys.
func (e moveEnv) Start(ctx context.Context, envID, nodeID string) error {
	o := e.o
	if cur, err := o.envNode(ctx, envID); err != nil {
		return err
	} else if cur != nodeID {
		if err := o.store.Environments.SetNode(ctx, envID, nodeID); err != nil {
			return err
		}
	}
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return err
	}
	log := moveLog(ctx)
	// the job's ctx names the node it started on; the deploy (image pulls
	// included) runs on the target
	ctx = docker.WithNode(ctx, nodeID)
	var done []string
	for _, t := range deployOrder(ts) {
		_, _ = fmt.Fprintf(log, "deploying %s on the new node\n", t.Slug)
		if err := o.deploy.Redeploy(ctx, t.ID, log, nil); err != nil {
			return fmt.Errorf("deploy %s: %w", t.Slug, err)
		}
		done = append(done, t.ID)
	}
	if err := o.afterDeploy(ctx, done, true); err != nil {
		return err
	}
	if err := o.sync.Sync(ctx); err != nil {
		return fmt.Errorf("proxy sync: %w", err)
	}
	return o.rebuildVIPs(ctx)
}

// Remove drops the env's containers (and VIP rules) on nodeID, never its
// volumes; a node the env no longer lives on loses its network too.
func (e moveEnv) Remove(ctx context.Context, envID, nodeID string) error {
	o := e.o
	env, err := o.store.Environments.Get(ctx, envID)
	if err != nil {
		return err
	}
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return err
	}
	nctx := docker.WithNode(ctx, nodeID)
	// No Node on this leaf: it must act on nodeID, not on the env's node.
	leaf := tile.New(o.store.Tiles, o.docker, o.vip)
	for _, t := range ts {
		if err := leaf.Teardown(nctx, t, env.Network); err != nil {
			return fmt.Errorf("remove %s: %w", t.Slug, err)
		}
	}
	if cur, _ := o.envNode(ctx, envID); cur != nodeID {
		if err := o.docker.RemoveNetwork(nctx, env.Network); err != nil {
			slog.Warn("move: old network kept", "env", env.Slug, "node", nodeID, "err", err)
		}
	}
	return nil
}

// deployOrder is the env's tiles managed first, then slices, then the rest,
// each group with depends_on first (a cycle keeps the stored order).
func deployOrder(ts []store.Tile) []store.Tile {
	pass := func(t store.Tile) int {
		switch t.Kind {
		case tile.Managed:
			return 0
		case tile.Slice:
			return 1
		}
		return 2
	}
	bySlug := map[string]store.Tile{}
	for _, t := range ts {
		bySlug[t.Slug] = t
	}
	var out []store.Tile
	seen := map[string]bool{}
	var visit func(t store.Tile)
	visit = func(t store.Tile) {
		if seen[t.Slug] {
			return
		}
		seen[t.Slug] = true
		for _, l := range tile.Lines(t.DependsOn) {
			if s, _, err := tile.ParseDep(l); err == nil {
				if d, ok := bySlug[s]; ok {
					visit(d)
				}
			}
		}
		out = append(out, t)
	}
	for p := range 3 {
		for _, t := range ts {
			if pass(t) == p {
				visit(t)
			}
		}
	}
	return out
}
