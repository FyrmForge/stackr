// Package runtime is the single Docker boundary: no other package may import
// the Docker SDK or shell out to docker. Everything speaks in Stackr terms
// (apps, deployments) so a Swarm backend can slot in later.
package runtime

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

const (
	NetworkName = "stkr"
	// PanelAlias is the DNS name the panel answers to on stkr, traefik's
	// route to it and the agent's way back to the panel are both this name.
	PanelAlias   = "stkr-panel"
	LabelApp     = "stackr.app"
	LabelDB      = "stackr.db"
	LabelManaged = "stackr.managed"
	LabelDeploy  = "stackr.deployment"
	// LabelRun marks a one-shot started for a cron_runs row; the value is the
	// run id. It is how the panel finds a run in flight to follow or kill.
	LabelRun = "stackr.run"

	// LabelAgentTask is the label the node agent's own service carries. Kept
	// here rather than read from infra/agent, which imports this package.
	// It marks the agent's container as one not to stop from the UI.
	LabelAgentTask = "stackr.agent"

	// LabelProxyRelay marks a forward-relay container; the value is the tile
	// id it relays to.
	LabelProxyRelay = "stackr.proxyrelay"
	// labelMove marks the receiving container of a volume move; the value is
	// the move id. See move.go.
	labelMove = "stackr.move"
	// MoveAliasPrefix is what a move's receiving container answers to on the
	// stkr overlay, suffixed with the move id. MovePort is the rsync daemon
	// inside it, published nowhere: both ends are on the overlay.
	MoveAliasPrefix = "stkr-move-"
	MovePort        = 8873
	// TraefikService is the swarm service the shared proxy runs as. Named
	// here because netpool has to put a grown pool network into its spec and
	// must not import infra/proxy.
	TraefikService = "stkr-traefik"
	// ProxyRelayImage is built by `make proxyrelay` (and the deploy script),
	// never built or pulled from here.
	ProxyRelayImage = "stkr-proxyrelay:local"
	// ProxyRelayPort is the fixed port every relay listens on; each relay has
	// its own network namespace, so there is nothing to collide with.
	ProxyRelayPort = 15000
)

type Runtime struct {
	cli *client.Client

	// relayMu serializes proxyrelay create/inspect/remove. Two concurrent
	// forwards to the same tile otherwise race the create-start-connect
	// window and the loser errors on a half-made container (seen live:
	// "No such container" from the second of two simultaneous CLI forwards).
	relayMu sync.Mutex

	// self is this process's own container id, resolved once (selfID).
	selfOnce sync.Once
	self     string
}

func New() (*Runtime, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Runtime{cli: cli}, nil
}

// EnsureOverlay creates an attachable swarm overlay if it is missing. Every
// tenant network is one of these (docs/plans/30-docker-swarm.md): a service
// cannot hot-join a network, so they are pre-created and handed out from a
// pool instead of being created per environment. Attachable so plain
// containers, traefik and the forward relays, until step 4, can still join.
// Requires a swarm manager; the install script inits one.
func (r *Runtime) EnsureOverlay(ctx context.Context, name string) error {
	nets, err := r.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if n.Name == name {
			return nil
		}
	}
	// Plain VXLAN, never encrypted. Docker fixes the flag at create and
	// cannot change it, so an encrypted overlay could only ever be flipped by
	// tearing down every service and network on the box and rebuilding them,
	// which is a job that deletes the panel's own service partway through.
	// Not worth it: join nodes over a VPN or a private network instead
	// (docs/plans/31-node-agent-open-questions.md, overlay encryption).
	_, err = r.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "overlay", Attachable: true,
	})
	// Two panels (or a retry) racing on the same name is fine; both wanted it.
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

// NetworkTenants lists the containers on a network that are not stackr's own
// infrastructure. Traefik sits on every tenant network, so a plain member
// count can never reach zero, this is what "empty" has to mean before a
// pooled network is handed to the next environment. A missing network counts
// as empty.
func (r *Runtime) NetworkTenants(ctx context.Context, name string) ([]string, error) {
	n, err := r.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for id := range n.Containers {
		if r.ContainerIsSystem(ctx, id) {
			continue
		}
		// Only something that really is a container. Docker lists the
		// overlay's own load-balancer sandbox among a network's endpoints
		// (named "<net>-endpoint"), it is not a container, and a disconnect
		// of it fails. That failure used to abort the whole drain, which
		// nothing noticed while the callers discarded the error: every
		// pooled overlay that had ever carried a service was undrainable.
		info, err := r.cli.ContainerInspect(ctx, id)
		if err != nil {
			continue
		}
		// A swarm task's network attachment lives in its service spec, not on
		// the container: disconnecting it is undone within seconds and the
		// only thing it achieves is a fight with the orchestrator. The service
		// is the handle, NetworkServices returns it.
		if info.Config != nil && info.Config.Labels[labelSwarmTaskID] != "" {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// NetworkServices lists the swarm services with a task on the named network,
// skipping protected infrastructure. A pool network no row holds any more with
// a service still on it is an orphan: the rows went and the service did not,
// and the next environment to claim that network would inherit a neighbour it
// can reach.
func (r *Runtime) NetworkServices(ctx context.Context, name string) ([]string, error) {
	// Verbose: without it an overlay inspect on the manager lists only the
	// containers on the manager, so a tenant living on a worker is invisible
	// and the boot sweep decides the network is clear when it is not.
	n, err := r.cli.NetworkInspect(ctx, name, network.InspectOptions{Verbose: true})
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, nil
		}
		return nil, err
	}
	// n.Services is the cluster-wide view Verbose exists for; n.Containers is
	// still only the manager's endpoints, so a worker-placed service would be
	// missed there and its network recycled under it.
	var selfSvc string
	if self := r.selfID(ctx); self != "" {
		if info, err := r.cli.ContainerInspect(ctx, self); err == nil && info.Config != nil {
			selfSvc = info.Config.Labels[labelSwarmService]
		}
	}
	var out []string
	for name := range n.Services {
		if name == "" || name == selfSvc {
			continue
		}
		svc, _, err := r.cli.ServiceInspectWithRaw(ctx, name, swarm.ServiceInspectOptions{})
		if err != nil {
			continue
		}
		var cl map[string]string
		if c := svc.Spec.TaskTemplate.ContainerSpec; c != nil {
			cl = c.Labels
		}
		if systemRoleLabel(svc.Spec.Labels) != "" || systemRoleLabel(cl) != "" {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// ListNetworks returns network names starting with prefix.
func (r *Runtime) ListNetworks(ctx context.Context, prefix string) ([]string, error) {
	nets, err := r.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nets {
		if strings.HasPrefix(n.Name, prefix) {
			out = append(out, n.Name)
		}
	}
	return out, nil
}

// NetworkMemberAddr returns a container's address on one network, as the bare
// IP plus the network's own subnet in CIDR form, the two things an app behind
// traefik needs to trust a forwarded header, either exactly (the IP) or for the
// life of the network (the subnet).
//
// Both come off a single inspect: docker reports a member's IPv4Address already
// suffixed with the network prefix, and the subnet is the IPAM config's. A
// container that is not on the network is not an error here, it is the state
// during a proxy restart, so it returns empty strings and lets the caller
// decide.
func (r *Runtime) NetworkMemberAddr(ctx context.Context, netName, containerID string) (ip, cidr string, err error) {
	n, err := r.cli.NetworkInspect(ctx, netName, network.InspectOptions{})
	if err != nil {
		return "", "", err
	}
	if len(n.IPAM.Config) > 0 {
		cidr = n.IPAM.Config[0].Subnet
	}
	m, ok := n.Containers[containerID]
	if !ok || m.IPv4Address == "" {
		return "", cidr, nil
	}
	// "172.18.0.4/16", the prefix here is the network's, not a per-container
	// one, so the subnet above stays the authority for the range.
	ip, _, _ = strings.Cut(m.IPv4Address, "/")
	return ip, cidr, nil
}

// DisconnectTenants takes every non-infrastructure container off a network
// and leaves the network standing. Pooled networks are reused by the next
// tenant (infra/netpool), so they are drained rather than removed; traefik and
// the panel stay attached. Returns how many were disconnected.
func (r *Runtime) DisconnectTenants(ctx context.Context, name string) (int, error) {
	ids, err := r.NetworkTenants(ctx, name)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		// Force: a running container is exactly the case this exists for.
		if err := r.cli.NetworkDisconnect(ctx, name, id, true); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// alreadyJoined recognises the two different sentences docker uses for "this
// container is on that network already". Treating either as an error is what
// made traefik abandon the rest of the pool halfway through a reattach, so
// every environment past the first already-joined one went unrouted.
func alreadyJoined(err error) bool {
	m := err.Error()
	return strings.Contains(m, "already exists") || strings.Contains(m, "already attached")
}

// ConnectContainer joins containerID to netName with the given DNS aliases
// (nil = plain client join). Already-connected is not an error. Used for
// shared-instance networks: the instance joins with its alias so consumers
// can resolve it, consumers join with no alias.
func (r *Runtime) ConnectContainer(ctx context.Context, netName, containerID string, aliases []string) error {
	var cfg *network.EndpointSettings
	if len(aliases) > 0 {
		cfg = &network.EndpointSettings{Aliases: aliases}
	}
	if err := r.cli.NetworkConnect(ctx, netName, containerID, cfg); err != nil && !alreadyJoined(err) {
		return err
	}
	return nil
}

// DialContainer opens a TCP connection to port on the tile's container,
// always through a proxyrelay: a disposable container on both the stackr
// network and the target's network, so stackr itself never
// joins environment networks. targetAlias is the tile's DNS alias
// (envnet.TileAlias, passed in because envnet imports this package), which
// the relay dials so a redeployed tile re-resolves.
//
// The relay can idle-exit between our inspect and our dial; on a failed dial
// the relay is removed and recreated once.
func (r *Runtime) DialContainer(ctx context.Context, tileID, containerID, targetAlias string, port int) (net.Conn, error) {
	info, err := r.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, err
	}
	if info.State == nil || !info.State.Running {
		return nil, fmt.Errorf("container is not running")
	}
	// The relay joins the network where the tile's alias resolves.
	var envNet string
	for name, n := range info.NetworkSettings.Networks {
		if slices.Contains(n.Aliases, targetAlias) {
			envNet = name
			break
		}
	}
	if envNet == "" {
		return nil, fmt.Errorf("container carries no %s alias on any network", targetAlias)
	}
	return r.DialOnNetwork(ctx, tileID, envNet, targetAlias, port)
}

// DialOnNetwork is DialContainer without the container: it puts the relay on
// the named network and dials the alias there.
//
// This is the multi-node half. Inspecting the tile's container to find its
// network only works while the container is on this machine, and a port
// forward to a tile on a worker died at that inspect with "no such container",
// even though the env network is an overlay and the alias resolves on it
// from any node, which is the whole point of an overlay. The caller passes
// the network from the environment row instead.
func (r *Runtime) DialOnNetwork(ctx context.Context, tileID, envNet, targetAlias string, port int) (net.Conn, error) {
	relayName := "stkr-proxyrelay-" + tileID[:8] + "-" + strconv.Itoa(port)
	target := net.JoinHostPort(targetAlias, strconv.Itoa(port))
	d := net.Dialer{Timeout: 2 * time.Second}
	for attempt := range 2 {
		ip, err := r.ensureProxyRelay(ctx, relayName, tileID, envNet, target)
		if err != nil {
			return nil, err
		}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(ProxyRelayPort)))
		if err == nil {
			return conn, nil
		}
		if attempt == 1 {
			return nil, fmt.Errorf("proxyrelay unreachable: %w", err)
		}
		// Idle-exit race, or a stale relay: recreate and retry once. A relay
		// that is still running is not stale, it is multiplexing somebody
		// else's live forward, and removing it to fix this dial would drop
		// their session. Fail this one instead.
		r.relayMu.Lock()
		info, ierr := r.cli.ContainerInspect(ctx, relayName)
		live := ierr == nil && info.State != nil && info.State.Running
		if !live {
			_ = r.cli.ContainerRemove(ctx, relayName, container.RemoveOptions{Force: true})
		}
		r.relayMu.Unlock()
		if live {
			return nil, fmt.Errorf("proxyrelay unreachable: %w", err)
		}
	}
	panic("unreachable")
}

// ensureProxyRelay finds or creates the named relay container and returns its
// IP on the stackr network. Created with AutoRemove and an idle timeout, so
// it reaps itself. Serialized: the second of two concurrent forwards waits
// out the first's create+start+connect and then reuses the result.
func (r *Runtime) ensureProxyRelay(ctx context.Context, name, tileID, envNet, target string) (string, error) {
	r.relayMu.Lock()
	defer r.relayMu.Unlock()
	relayIP := func() (string, error) {
		info, err := r.cli.ContainerInspect(ctx, name)
		if err != nil {
			return "", err
		}
		if info.State == nil || !info.State.Running {
			return "", fmt.Errorf("relay %s is not running", name)
		}
		// The relay must also be on the target's network; already-joined is
		// fine. Covers both reuse and the create path below.
		if err := r.ConnectContainer(ctx, envNet, info.ID, nil); err != nil {
			return "", err
		}
		n, ok := info.NetworkSettings.Networks[NetworkName]
		if !ok || n.IPAddress == "" {
			return "", fmt.Errorf("relay %s has no address on the %s network", name, NetworkName)
		}
		return n.IPAddress, nil
	}

	if ip, err := relayIP(); err == nil {
		return ip, nil
	}
	create := func() error {
		_, err := r.cli.ContainerCreate(ctx,
			&container.Config{
				Image:  ProxyRelayImage,
				Cmd:    []string{"-target", target},
				Labels: map[string]string{LabelManaged: "true", LabelProxyRelay: tileID},
			},
			&container.HostConfig{AutoRemove: true},
			&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
				NetworkName: {},
			}},
			nil, name)
		if err != nil {
			return err
		}
		if err := r.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
			_ = r.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
			return err
		}
		return nil
	}
	err := create()
	switch {
	case err == nil:
	case strings.Contains(err.Error(), "No such image"):
		return "", fmt.Errorf("proxyrelay image %s missing; run `make proxyrelay` (or redeploy)", ProxyRelayImage)
	case strings.Contains(err.Error(), "is already in use"):
		// The name is taken by a relay that relayIP just found absent or
		// stopped, it is mid-AutoRemove from its idle exit. It cannot be a
		// live sibling: relayIP would have returned that one, and relayMu means
		// no create from this process is in flight. So clear the name and make
		// the new one, which is the recreate DialContainer's doc promises.
		_ = r.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true})
		if err := create(); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	return relayIP()
}

// SweepForwardState cleans up after the pre-proxyrelay design and crashes:
// removes leftover relay containers (the normal path is their own idle exit)
// and detaches the stackr container itself from every non-stackr network the
// shipped self-attach version joined. Best-effort; under `hamr dev` the
// process has no container and the detach half is a no-op.
func (r *Runtime) SweepForwardState(ctx context.Context) {
	cs, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", LabelProxyRelay)),
	})
	if err == nil {
		for _, c := range cs {
			_ = r.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		}
	}

	self, err := os.Hostname()
	if err != nil {
		return
	}
	info, err := r.cli.ContainerInspect(ctx, self)
	if err != nil {
		return // not running in a container
	}
	for name := range info.NetworkSettings.Networks {
		if name != NetworkName && name != "bridge" {
			_ = r.cli.NetworkDisconnect(ctx, name, info.ID, true)
		}
	}
}

// EnsureBuilder creates a buildx docker-container builder called name if
// this panel does not know it yet, capped at memMB with a low cpu weight so
// a build yields to running services instead of pinning the manager. The
// daemon's own builder ignores every resource flag. One builder per org,
// each with its own cache, so no org ever hits another's cached step
// (docs/plans/37-builds.md, items 2 and 3). An existing builder container
// of that name is reused.
func (r *Runtime) EnsureBuilder(ctx context.Context, name string, memMB int) error {
	if exec.CommandContext(ctx, "docker", "buildx", "inspect", name).Run() == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "buildx", "create", "--name", name,
		"--driver", "docker-container",
		"--driver-opt", fmt.Sprintf("memory=%dm", memMB),
		"--driver-opt", "cpu-shares=256").CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildx create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// EnsureRemoteBuilder points a buildx builder called name at a buildkit
// daemon listening on addr (docs/plans/37-builds.md, item 4). The daemon is
// somebody else's to run; this is only the client side.
func (r *Runtime) EnsureRemoteBuilder(ctx context.Context, name, addr string) error {
	// An existing builder may point at an address we no longer use: the build
	// node's buildkit moved off its published port onto the overlay alias, and
	// inspect alone would keep the stale endpoint forever. Recreate on a
	// mismatch; inspect prints the endpoint as its own line.
	if out, err := exec.CommandContext(ctx, "docker", "buildx", "inspect", name).CombinedOutput(); err == nil {
		if strings.Contains(string(out), addr) {
			return nil
		}
		if err := r.RemoveBuilder(ctx, name); err != nil {
			return err
		}
	}
	out, err := exec.CommandContext(ctx, "docker", "buildx", "create", "--name", name,
		"--driver", "remote", addr).CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildx create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveBuilder removes a builder and, for a docker-container one, its
// container and cache volume. Missing is fine.
func (r *Runtime) RemoveBuilder(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, "docker", "buildx", "rm", "--force", name).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "no builder") {
		return fmt.Errorf("buildx rm: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// BuildImage builds dir with the given dockerfile into tag, streaming build
// output to logW. Shells out to the docker CLI for full BuildKit fidelity.
// Runs in the named builder and loads the result into the daemon, where the
// push that follows expects it.
func (r *Runtime) BuildImage(ctx context.Context, builder, dir, dockerfile, tag string, buildArgs, labels map[string]string, logW io.Writer) error {
	args := []string{"build", "--builder", builder, "--load", "-f", dockerfile, "-t", tag, "--progress=plain"}
	for k, v := range buildArgs {
		args = append(args, "--build-arg", k+"="+v)
	}
	for k, v := range labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, ".")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	cmd.Stdout = logW
	cmd.Stderr = logW
	return cmd.Run()
}

// PullImage pulls ref, streaming progress to logW.
func (r *Runtime) PullImage(ctx context.Context, ref string, logW io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "pull", ref)
	cmd.Stdout = logW
	cmd.Stderr = logW
	return cmd.Run()
}

// LocalDigest returns the manifest digest the local image for ref was pulled
// from ("sha256:..."), "" if the image was built locally or never pulled.
func (r *Runtime) LocalDigest(ctx context.Context, ref string) (string, error) {
	info, err := r.cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", err
	}
	repoName := ref
	if i := strings.LastIndex(repoName, ":"); i > strings.LastIndex(repoName, "/") {
		repoName = repoName[:i] // strip the tag, keep a registry port
	}
	for _, rd := range info.RepoDigests {
		if strings.HasPrefix(rd, repoName+"@") {
			return rd[len(repoName)+1:], nil
		}
	}
	// Tagged into another repo locally, any pulled digest beats none.
	for _, rd := range info.RepoDigests {
		if i := strings.Index(rd, "@"); i >= 0 {
			return rd[i+1:], nil
		}
	}
	return "", nil
}

// PushImage pushes ref, streaming progress to logW.
func (r *Runtime) PushImage(ctx context.Context, ref string, logW io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "push", ref)
	cmd.Stdout = logW
	cmd.Stderr = logW
	return cmd.Run()
}

// TagImage tags src as dst.
func (r *Runtime) TagImage(ctx context.Context, src, dst string) error {
	return r.cli.ImageTag(ctx, src, dst)
}

func (r *Runtime) RegistryLogin(ctx context.Context, url, user, pass string) error {
	cmd := exec.CommandContext(ctx, "docker", "login", url, "-u", user, "--password-stdin")
	cmd.Stdin = strings.NewReader(pass)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker login %s: %s", url, strings.TrimSpace(string(out)))
	}
	return nil
}

// ContainerSpec describes a container to run on the stackr network.
type ContainerSpec struct {
	Name        string
	Image       string
	Cmd         []string // override image CMD (nil = image default)
	Env         []string // KEY=VALUE
	Labels      map[string]string
	Volumes     []string          // "name-or-hostpath:/container/path"
	Ports       map[string]string // hostPort -> containerPort (published)
	Aliases     []string          // stable DNS names on the network
	NetworkName string            // defaults to stackr network
	CPULimit    float64           // cores, 0 = unlimited
	MemLimitMB  int               // MB, 0 = unlimited

	User          string   // docker --user, "uid[:gid]" ("" = image default)
	ShmSizeMB     int      // /dev/shm size, 0 = docker default (64MB)
	Privileged    bool     // full host access, the caller gates who may set it
	Devices       []string // "host[:container[:perms]]" per entry
	RestartPolicy string   // "" | "on-failure" | "no" (runtime.NormalizeRestart)

	// Docker-native HEALTHCHECK: HealthCmd runs as CMD-SHELL on the knobs
	// below (each 0 = docker default). Empty HealthCmd = no healthcheck.
	HealthCmd          string
	HealthIntervalS    int
	HealthTimeoutS     int
	HealthRetries      int
	HealthStartPeriodS int
}

// ParseDevice decodes one device line into docker's mapping. The container
// path defaults to the host path and permissions to rwm, matching compose.
func ParseDevice(line string) (container.DeviceMapping, error) {
	parts := strings.SplitN(line, ":", 3)
	d := container.DeviceMapping{PathOnHost: parts[0], PathInContainer: parts[0], CgroupPermissions: "rwm"}
	if len(parts) > 1 && parts[1] != "" {
		d.PathInContainer = parts[1]
	}
	if len(parts) > 2 && parts[2] != "" {
		d.CgroupPermissions = parts[2]
	}
	if !strings.HasPrefix(d.PathOnHost, "/") || !strings.HasPrefix(d.PathInContainer, "/") {
		return d, fmt.Errorf("device %q: paths must be absolute (host[:container[:perms]])", line)
	}
	return d, nil
}

// RunContainer creates and starts a container, returning its ID.
func (r *Runtime) RunContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	if spec.NetworkName == "" {
		spec.NetworkName = NetworkName
	}
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels[LabelManaged] = "true"

	exposed, bindings, err := portBindings(spec.Ports)
	if err != nil {
		return "", err
	}

	var devices []container.DeviceMapping
	for _, l := range spec.Devices {
		d, err := ParseDevice(l)
		if err != nil {
			return "", err
		}
		devices = append(devices, d)
	}
	restart := container.RestartPolicyUnlessStopped
	if spec.RestartPolicy == "always" {
		restart = container.RestartPolicyAlways
	}

	cfg := &container.Config{
		Image:        spec.Image,
		Cmd:          spec.Cmd,
		Env:          spec.Env,
		User:         spec.User,
		Labels:       spec.Labels,
		ExposedPorts: exposed,
	}
	if spec.HealthCmd != "" {
		// Interval defaults to 5s, not docker's 30s: the deploy gate waits on
		// the first native check, and a 30s floor on every deploy is a worse
		// default than a slightly chattier probe.
		interval := 5
		if spec.HealthIntervalS > 0 {
			interval = spec.HealthIntervalS
		}
		cfg.Healthcheck = &container.HealthConfig{
			Test:        []string{"CMD-SHELL", spec.HealthCmd},
			Interval:    time.Duration(interval) * time.Second,
			Timeout:     time.Duration(spec.HealthTimeoutS) * time.Second,
			Retries:     spec.HealthRetries,
			StartPeriod: time.Duration(spec.HealthStartPeriodS) * time.Second,
		}
	}
	hostCfg := &container.HostConfig{
		Binds:         spec.Volumes,
		PortBindings:  bindings,
		RestartPolicy: container.RestartPolicy{Name: restart},
		Privileged:    spec.Privileged,
		ShmSize:       int64(spec.ShmSizeMB) << 20,
		Resources: container.Resources{
			NanoCPUs: int64(spec.CPULimit * 1e9),
			Memory:   int64(spec.MemLimitMB) << 20,
			Devices:  devices,
		},
	}
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			spec.NetworkName: {Aliases: spec.Aliases},
		},
	}

	resp, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, spec.Name)
	if err != nil {
		return "", err
	}
	if err := r.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = r.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", err
	}
	return resp.ID, nil
}

// ExecStream runs cmd inside the container with optional stdin, streaming
// stdout to the returned reader. Caller must drain and close via the returned
// wait func, which reports the exec's error (stderr included in the message).
func (r *Runtime) ExecStream(ctx context.Context, containerID string, cmd []string, stdin io.Reader) (io.Reader, func() error, error) {
	// The Go client, not `docker exec`: this call also runs inside the node
	// agent, whose image ships no docker CLI
	// (docs/plans/31-node-agent-open-questions.md, the volume tool).
	execID, err := r.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, nil, err
	}
	att, err := r.cli.ContainerExecAttach(ctx, execID.ID, container.ExecStartOptions{})
	if err != nil {
		return nil, nil, err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, stdin)
			if cw, ok := att.Conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}()
	}
	// No TTY here (that is ExecTTY), so the stream carries docker's
	// multiplexed frames and stdout has to be demuxed out of them. stderr is
	// kept for the error message, exactly as the CLI version reported it.
	pr, pw := io.Pipe()
	var stderr strings.Builder
	copyDone := make(chan struct{})
	go func() {
		_, err := stdcopy.StdCopy(pw, &stderr, att.Reader)
		_ = pw.CloseWithError(err)
		close(copyDone)
	}()
	wait := func() error {
		// Close the read side first, the same fix toolContainer carries. A
		// caller that abandoned the stream leaves StdCopy blocked writing into
		// the pipe, and waiting on copyDone before unblocking it hangs here
		// forever, holding the exec and its container open with it.
		_ = pr.CloseWithError(io.ErrClosedPipe)
		<-copyDone
		att.Close()
		insp, err := r.cli.ContainerExecInspect(context.WithoutCancel(ctx), execID.ID)
		if err != nil {
			return err
		}
		// Running means the exec has not finished, so its ExitCode is zero
		// because there is no exit yet, not because it succeeded. Reading it
		// as success is how an interrupted database dump came back green.
		if insp.Running {
			return fmt.Errorf("the command was still running when its output ended: %s",
				strings.TrimSpace(stderr.String()))
		}
		if insp.ExitCode != 0 {
			return fmt.Errorf("exit status %d: %s", insp.ExitCode, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	return pr, wait, nil
}

// ExecTTY opens an interactive TTY in the container running cmd (nil = a
// login shell). Returns a raw read/write stream bridged to the exec's TTY,
// plus a resize func and closer.
func (r *Runtime) ExecTTY(ctx context.Context, containerID string, cmd []string) (io.ReadWriter, func(w, h uint) error, func(), error) {
	if cmd == nil {
		cmd = []string{"sh", "-c", "command -v bash >/dev/null && exec bash || exec sh"}
	}
	execResp, err := r.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	att, err := r.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{Tty: true})
	if err != nil {
		return nil, nil, nil, err
	}
	resize := func(w, h uint) error {
		return r.cli.ContainerExecResize(ctx, execResp.ID, container.ResizeOptions{Width: w, Height: h})
	}
	return att.Conn, resize, att.Close, nil
}

func randomSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- volume file access ---
//
// Every op mounts the named volume into a throwaway busybox-based container,
// so it behaves the same whether the container using the volume is running or
// stopped, and never depends on what binaries the db image ships.
// first use pulls alpine; docker caches it after that.

// VolumeToolImage is the throwaway container every volume op runs in. alpine
// on the manager, which pulls it once; the agent points it at its own image
// at startup, because a worker has no reason to have alpine and a volume
// browse is not the moment to pull one.
var VolumeToolImage = "alpine:3"

// VolumeEntry is one file or directory in a volume listing.
type VolumeEntry struct {
	Name    string
	Dir     bool
	Size    int64
	ModTime time.Time
}

// volumePath confines a user-supplied path to the volume mount. Clean on a
// /-rooted path squeezes out any "..", so the result is always under /data.
func volumePath(p string) string {
	return path.Join("/data", path.Clean("/"+p))
}

// volumeTool runs cmd in a throwaway container with vol mounted at /data and
// hands back the command's stdout plus a wait func that reports its exit.
//
// Everything below goes through here rather than shelling out to `docker run`,
// so the agent image can ship without a docker CLI in it
// (docs/plans/31-node-agent-open-questions.md, the volume tool). Same image,
// same argv, same container as before, only the way it is started changed.
//
// The caller must call wait even on a read it abandons: that is what removes
// the container.
func (r *Runtime) volumeTool(ctx context.Context, vol string, ro bool, cmd []string, stdin io.Reader) (io.Reader, func() error, error) {
	// Here rather than in each caller. VolumeToolImage is only replaced by a
	// real local image when agent.Ensure runs, and on a single-node install
	// that never happens, so the default alpine has to be pulled, and a box
	// that had never pulled one answered every volume size, listing and browse
	// with "No such image: alpine:3". Backup and
	// storagetiles remembered to call this; nothing else did.
	//
	// One cached ImageInspect per volume op once the image is there.
	if err := r.EnsureVolumeTool(ctx); err != nil {
		return nil, nil, err
	}
	mnt := vol + ":/data"
	if ro {
		mnt += ":ro"
	}
	return r.toolContainer(ctx, toolOpts{
		image: VolumeToolImage, cmd: cmd, binds: []string{mnt},
		stdin: stdin, name: "stkr-vol-" + randomSuffix(),
	})
}

// toolOpts describes one throwaway container.
type toolOpts struct {
	image string
	cmd   []string
	binds []string
	stdin io.Reader
	name  string
	// network, when set, is the docker network the container joins. Only the
	// volume move needs one: its two containers talk to each other.
	network string
	// labels are merged onto the container. The move's containers carry the
	// move id so a sweep can find them after a crash.
	labels map[string]string
}

// toolLabels stamps every throwaway container as ours, plus whatever the
// caller adds.
func toolLabels(extra map[string]string) map[string]string {
	out := map[string]string{LabelManaged: "true"}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// toolContainer starts a throwaway container and hands back its stdout plus a
// wait func reporting its exit.
func (r *Runtime) toolContainer(ctx context.Context, o toolOpts) (io.Reader, func() error, error) {
	stdin := o.stdin
	var netCfg *network.NetworkingConfig
	if o.network != "" {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{o.network: {}},
		}
	}
	created, err := r.cli.ContainerCreate(ctx,
		&container.Config{
			Image: o.image,
			// Cleared, not inherited. The move runs in the agent's own image,
			// whose ENTRYPOINT is the stackrd binary, leaving it in place
			// turns `sh -c rsync ...` into arguments to stackrd, which exits
			// 1 without copying anything.
			Entrypoint:   strslice.StrSlice{},
			Cmd:          o.cmd,
			AttachStdin:  stdin != nil,
			OpenStdin:    stdin != nil,
			StdinOnce:    stdin != nil,
			AttachStdout: true,
			AttachStderr: true,
			Labels:       toolLabels(o.labels),
		},
		&container.HostConfig{Binds: o.binds, AutoRemove: false},
		netCfg, nil, o.name)
	if err != nil {
		return nil, nil, err
	}
	// AutoRemove is off on purpose: the exit code has to be readable after the
	// process ends, and a self-removing container races ContainerWait.
	remove := func() {
		_ = r.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}

	att, err := r.cli.ContainerAttach(ctx, created.ID, container.AttachOptions{
		Stream: true, Stdin: stdin != nil, Stdout: true, Stderr: true,
	})
	if err != nil {
		remove()
		return nil, nil, err
	}

	// Wait on the container before starting it: docker's own docs call the
	// other order a race, and a command that exits immediately (an empty
	// `cat`) is exactly the case that loses the event.
	//
	// NextExit, not NotRunning. A container that has been created and not yet
	// started is already "not running", so that condition is satisfied at
	// once and returns status 0, every failure of every tool container read
	// as success, including an rsync that copied nothing.
	okC, errC := r.cli.ContainerWait(context.WithoutCancel(ctx), created.ID, container.WaitConditionNextExit)

	if err := r.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		att.Close()
		remove()
		return nil, nil, err
	}

	if stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, stdin)
			// Half-close so the command sees EOF on stdin; a full Close would
			// take the output stream down with it.
			if cw, ok := att.Conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}()
	}

	// No TTY, so the attach stream is docker's multiplexed frame format and
	// has to be demuxed. stderr is buffered for the error message; stdout is
	// piped to the caller.
	pr, pw := io.Pipe()
	var stderr strings.Builder
	copyDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(pw, &stderr, att.Reader)
		_ = pw.CloseWithError(err)
		copyDone <- err
	}()

	wait := func() error {
		// Close the read side first. A caller that abandoned the stream, a
		// tar whose io.Copy failed, or an agent whose HTTP client hung up,
		// leaves StdCopy blocked writing into the pipe, and waiting on
		// copyDone before unblocking it hangs here forever, holding the
		// container and the volume open with it.
		_ = pr.CloseWithError(io.ErrClosedPipe)
		<-copyDone
		att.Close()
		defer remove()
		select {
		case err := <-errC:
			return err
		case st := <-okC:
			if st.Error != nil && st.Error.Message != "" {
				return fmt.Errorf("%s", st.Error.Message)
			}
			if st.StatusCode != 0 {
				msg := strings.TrimSpace(stderr.String())
				if msg == "" {
					msg = fmt.Sprintf("exit status %d", st.StatusCode)
				}
				return fmt.Errorf("%s", msg)
			}
			return nil
		}
	}
	return pr, wait, nil
}

// volumeToolRun is volumeTool for the commands whose output is only wanted
// when they fail.
func (r *Runtime) volumeToolRun(ctx context.Context, vol string, ro bool, cmd []string, stdin io.Reader) (string, error) {
	out, wait, err := r.volumeTool(ctx, vol, ro, cmd, stdin)
	if err != nil {
		return "", err
	}
	b, _ := io.ReadAll(out)
	return string(b), wait()
}

// ListVolumeFiles lists the entries directly under dir inside the volume
// ("/" = volume root), directories first.
func (r *Runtime) ListVolumeFiles(ctx context.Context, vol, dir string) ([]VolumeEntry, error) {
	out, err := r.volumeToolRun(ctx, vol, true, []string{
		"find", volumePath(dir), "-maxdepth", "1", "-mindepth", "1",
		"-exec", "stat", "-c", "%F|%s|%Y|%n", "{}", "+"}, nil)
	if err != nil {
		return nil, fmt.Errorf("list %q: %s", dir, strings.TrimSpace(err.Error()))
	}
	var entries []VolumeEntry
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue // a filename holding "\n" corrupts its row; db data dirs don't
		}
		size, _ := strconv.ParseInt(parts[1], 10, 64)
		mtime, _ := strconv.ParseInt(parts[2], 10, 64)
		entries = append(entries, VolumeEntry{
			Name:    path.Base(parts[3]),
			Dir:     parts[0] == "directory",
			Size:    size,
			ModTime: time.Unix(mtime, 0),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// ReadVolumeFile streams one file out of the volume. The returned closer
// reaps the docker process; the error it reports includes stderr.
func (r *Runtime) ReadVolumeFile(ctx context.Context, vol, file string) (io.ReadCloser, func() error, error) {
	out, wait, err := r.volumeTool(ctx, vol, true, []string{"cat", volumePath(file)}, nil)
	if err != nil {
		return nil, nil, err
	}
	return io.NopCloser(out), wait, nil
}

// WriteVolumeFile streams src into a file inside the volume, creating parent
// directories as needed.
func (r *Runtime) WriteVolumeFile(ctx context.Context, vol, file string, src io.Reader) error {
	dst := volumePath(file)
	// The path goes in as positional arguments, not interpolated into the
	// script. Go's %q escapes quotes and backslashes but not $ or backticks,
	// both of which are live inside shell double quotes, an uploaded file
	// named `x$(...)` would have run that command in here, with the volume
	// mounted writable. Every other volume op passes paths as argv already.
	if _, err := r.volumeToolRun(ctx, vol, false, []string{
		"sh", "-c", `mkdir -p "$1" && cat > "$2"`, "sh", path.Dir(dst), dst}, src); err != nil {
		return fmt.Errorf("write %q: %s", file, strings.TrimSpace(err.Error()))
	}
	return nil
}

// DeleteVolumeFile removes a file or directory (recursively) from the volume.
// The volume root itself is refused.
func (r *Runtime) DeleteVolumeFile(ctx context.Context, vol, file string) error {
	dst := volumePath(file)
	if dst == "/data" {
		return fmt.Errorf("refusing to delete the volume root")
	}
	if _, err := r.volumeToolRun(ctx, vol, false, []string{"rm", "-rf", dst}, nil); err != nil {
		return fmt.Errorf("delete %q: %s", file, strings.TrimSpace(err.Error()))
	}
	return nil
}

// EnsureVolumeTool pulls the helper image if it is not on the host yet. Call
// it before freezing a container: the first TarVolume on a fresh host would
// otherwise pull inside the pause window, turning "frozen for seconds" into
// "frozen for a registry round trip".
func (r *Runtime) EnsureVolumeTool(ctx context.Context) error {
	if _, err := r.cli.ImageInspect(ctx, VolumeToolImage); err == nil {
		return nil
	}
	return r.PullImage(ctx, VolumeToolImage, io.Discard)
}

// TarVolume streams a gzipped tar of the whole volume into w. It goes through
// the helper container's stdout rather than a shared path on purpose: stackr
// itself usually runs containerized, so a directory it can write is not a
// directory the docker daemon can bind-mount.
func (r *Runtime) TarVolume(ctx context.Context, vol string, w io.Writer, live bool) error {
	out, wait, err := r.volumeTool(ctx, vol, true,
		[]string{"tar", "-czf", "-", "-C", "/data", "."}, nil)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, out); err != nil {
		_ = wait()
		return fmt.Errorf("tar %s: %w", vol, err)
	}
	if err := wait(); err != nil {
		if live && isTarChanged(err) {
			// The documented torn-copy mode: the caller chose not to stop the
			// tile, so a file moving under the reader is what it asked for.
			// Every other tar failure still fails the backup.
			slog.Warn("backup: files changed while copying a running tile", "volume", vol, "tar", err)
			return nil
		}
		return fmt.Errorf("tar %s: %w", vol, err)
	}
	return nil
}

// isTarChanged reports whether a tar failure is only "the file moved under
// me". busybox says "short read", GNU says "file changed as we read it"; both
// mean the archive is intact except for that file.
func isTarChanged(err error) bool {
	m := err.Error()
	return strings.Contains(m, "short read") || strings.Contains(m, "file changed as we read it")
}

// UntarVolume wipes the volume and extracts the archive from r into it. The
// wipe is the destructive half of a restore, callers verify the archive
// first, because there is no way back from an emptied volume.
func (r *Runtime) UntarVolume(ctx context.Context, vol string, src io.Reader) error {
	if _, err := r.volumeToolRun(ctx, vol, false, []string{
		"sh", "-c", "rm -rf /data/..?* /data/.[!.]* /data/* 2>/dev/null; tar -xzf - -C /data"}, src); err != nil {
		return fmt.Errorf("restore into %s: %s", vol, strings.TrimSpace(err.Error()))
	}
	return nil
}

// VerifyTar reads an archive end to end and reports whether it is intact. A
// truncated or corrupt download fails here, before the volume is wiped.
func (r *Runtime) VerifyTar(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		if _, err := tr.Next(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("corrupt archive: %w", err)
		}
	}
}

// HostInfo is a snapshot of the Docker daemon for the servers screen.
type HostInfo struct {
	ServerVersion     string
	OS                string
	Arch              string
	NCPU              int
	MemTotal          int64
	Containers        int
	ContainersRunning int
	Images            int
}

// Info reports the Docker daemon snapshot.
func (r *Runtime) Info(ctx context.Context) (HostInfo, error) {
	info, err := r.cli.Info(ctx)
	if err != nil {
		return HostInfo{}, err
	}
	return HostInfo{
		ServerVersion:     info.ServerVersion,
		OS:                info.OperatingSystem,
		Arch:              info.Architecture,
		NCPU:              info.NCPU,
		MemTotal:          info.MemTotal,
		Containers:        info.Containers,
		ContainersRunning: info.ContainersRunning,
		Images:            info.Images,
	}, nil
}

// Exec runs cmd inside the container and returns combined output.
func (r *Runtime) Exec(ctx context.Context, containerID string, cmd []string) (string, error) {
	return r.execCombined(ctx, containerID, cmd)
}

// execCombined runs cmd in the container and returns stdout and stderr
// interleaved, the way `docker exec` printed them. The Go client rather than
// the CLI because the node agent runs this on its own node and its image
// ships no docker binary (docs/plans/31-node-agent-open-questions.md).
func (r *Runtime) execCombined(ctx context.Context, containerID string, cmd []string) (string, error) {
	execID, err := r.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd: cmd, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return "", err
	}
	att, err := r.cli.ContainerExecAttach(ctx, execID.ID, container.ExecStartOptions{})
	if err != nil {
		return "", err
	}
	defer att.Close()
	var buf bytes.Buffer
	// Both streams into one buffer: callers here show output to a human, and
	// splitting them would reorder a command's own progress against its
	// errors.
	if _, err := stdcopy.StdCopy(&buf, &buf, att.Reader); err != nil {
		return buf.String(), err
	}
	insp, err := r.cli.ContainerExecInspect(context.WithoutCancel(ctx), execID.ID)
	if err != nil {
		return buf.String(), err
	}
	if insp.ExitCode != 0 {
		return buf.String(), fmt.Errorf("exit status %d", insp.ExitCode)
	}
	return buf.String(), nil
}

// ExecShell runs a shell command inside the container. On ctx timeout the
// process group is killed inside the container too, killing just the docker
// CLI would leave the command running.
func (r *Runtime) ExecShell(ctx context.Context, containerID, command string) (string, error) {
	pidFile := "/tmp/.stackr-exec-" + randomSuffix() + ".pid"
	wrapped := fmt.Sprintf("echo $$ > %s; %s", pidFile, command)
	out, err := r.execCombined(ctx, containerID, []string{"sh", "-c", wrapped})
	if ctx.Err() != nil {
		// The context that timed out cannot carry the kill, so the cleanup
		// runs on a fresh one.
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		kill := fmt.Sprintf("kill -9 -$(cat %s) 2>/dev/null; kill -9 $(cat %s) 2>/dev/null; rm -f %s", pidFile, pidFile, pidFile)
		_, _ = r.execCombined(kctx, containerID, []string{"sh", "-c", kill})
		return out, fmt.Errorf("run timed out: %w", ctx.Err())
	}
	_, _ = r.execCombined(ctx, containerID, []string{"rm", "-f", pidFile})
	return out, err
}

// IsRunning reports whether the container is in running state.
func (r *Runtime) IsRunning(ctx context.Context, containerID string) (bool, error) {
	info, err := r.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return false, err
	}
	return info.State != nil && info.State.Running, nil
}

// ManagedContainer is a summary of a container Stackr knows about.
type ManagedContainer struct {
	ID    string
	Name  string
	Image string
	State string
	// Health is docker's health-status word (healthy | unhealthy | starting),
	// "" when the container declares no HEALTHCHECK. Parsed from the list
	// summary's human status string, no extra inspect per container.
	Health string
	Labels map[string]string
	IPs    []string // current addresses across all attached networks
	// System marks infrastructure that must not be stopped/removed from the
	// UI: the panel itself ("panel"), the node agent ("agent"), the shared
	// Traefik proxy ("proxy") or the image registry ("registry"). Doing so
	// would lock the operator out, cut a node off, kill all routing, or break
	// the next deploy to a worker.
	System string

	// Swarm task identity, read off the labels docker puts on every task
	// container. Slot is the stable handle, replica 2 is slot 2 for the life
	// of the service, while TaskID and this container's ID change on every
	// restart. All zero/empty on a plain container.
	Slot   int
	TaskID string
	NodeID string
}

// Swarm's own labels on a task's container. Docker sets them; nothing here
// writes them.
const (
	labelSwarmSlot   = "com.docker.swarm.task.slot"
	labelSwarmTaskID = "com.docker.swarm.task.id"
	labelSwarmNodeID = "com.docker.swarm.node.id"
	// labelSwarmService names the service a task belongs to. The only way back
	// from a stray container to the thing that keeps re-creating it.
	labelSwarmService = "com.docker.swarm.service.name"
)

// withTask fills in the swarm identity of a container from its labels.
func withTask(c ManagedContainer) ManagedContainer {
	c.Slot, _ = strconv.Atoi(c.Labels[labelSwarmSlot])
	c.TaskID, c.NodeID = c.Labels[labelSwarmTaskID], c.Labels[labelSwarmNodeID]
	return c
}

// healthWord extracts docker's health status from the list summary's human
// status string, "Up 2 minutes (unhealthy)", "Up 5 seconds (health: starting)".
func healthWord(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

// HealthStatus reads a container's live health via inspect: healthy |
// unhealthy | starting, or "" when no HEALTHCHECK is declared.
func (r *Runtime) HealthStatus(ctx context.Context, containerID string) (string, error) {
	info, err := r.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", err
	}
	if info.State == nil || info.State.Health == nil {
		return "", nil
	}
	return info.State.Health.Status, nil
}

// ParseFileMount decodes one tile files: line into its parts and validates
// the shape: a clean repo-relative source (no escape upward) and an absolute
// container path, with an optional trailing :template flag. Lives here
// beside ParseDevice so the deploy engine and the config layer share one
// grammar without an import cycle.
func ParseFileMount(line string) (repoPath, containerPath string, template bool, err error) {
	rest := line
	if strings.HasSuffix(rest, ":template") {
		template = true
		rest = strings.TrimSuffix(rest, ":template")
	}
	i := strings.IndexByte(rest, ':')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false, fmt.Errorf("files %q: want repo/path:/container/path[:template]", line)
	}
	repoPath, containerPath = rest[:i], rest[i+1:]
	clean := path.Clean(repoPath)
	if strings.HasPrefix(clean, "..") || path.IsAbs(clean) || clean == "." {
		return "", "", false, fmt.Errorf("files %q: source must be a path inside the repo", line)
	}
	if !strings.HasPrefix(containerPath, "/") {
		return "", "", false, fmt.Errorf("files %q: container path must be absolute", line)
	}
	return clean, containerPath, template, nil
}

// systemRole classifies a container as protected infrastructure: the node
// agent, the running panel (matched by this process's own container id), the
// shared Traefik proxy, or the image registry. "" = ordinary.
func (r *Runtime) systemRole(ctx context.Context, id string, labels map[string]string) string {
	// Labels first: they say what the container is, are free to read, and the
	// self check below only says who is asking. In the agent's own process
	// every list goes through the agent's container, which would otherwise
	// call itself the panel.
	if role := systemRoleLabel(labels); role != "" {
		return role
	}
	// Our own container, whichever process this is. Not the hostname: the
	// panel runs with --hostname stkr-panel so its overlay alias resolves, so
	// the old prefix check never matched and the containers page offered a
	// stop button on the panel itself, one click from locking the operator
	// out of the box (runtime.SelfContainerID).
	if self := r.selfID(ctx); self != "" && strings.HasPrefix(id, self) {
		return "panel"
	}
	return ""
}

// systemRoleLabel is the half of systemRole that needs no daemon: the roles a
// container declares about itself. Separate so it can be checked without a
// docker client, and because reading a map is cheaper than an inspect.
func systemRoleLabel(labels map[string]string) string {
	switch {
	case labels[LabelAgentTask] == "true":
		return "agent"
	case labels["stackr.traefik"] == "true":
		return "proxy"
	// The registry every worker pulls tile images from, and the one the
	// manager's builds push to. Stopping it from the containers page broke
	// the next deploy to a worker with a pull error that never mentioned the
	// registry.
	case labels["stackr.registry"] == "true":
		return "registry"
	}
	return ""
}

// selfID is this process's own container, looked up once. Every container
// listing asks, and the answer cannot change while the process lives.
func (r *Runtime) selfID(ctx context.Context) string {
	r.selfOnce.Do(func() { r.self = SelfContainerID(ctx, r) })
	return r.self
}

// ContainerIsSystem reports whether stopping/removing the container would take
// down protected infrastructure. Used to guard the container actions.
func (r *Runtime) ContainerIsSystem(ctx context.Context, id string) bool {
	info, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return false
	}
	return r.systemRole(ctx, info.ID, info.Config.Labels) != ""
}

// ListAll lists every container on the host (running or not).
func (r *Runtime) ListAll(ctx context.Context) ([]ManagedContainer, error) {
	cs, err := r.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	var out []ManagedContainer
	for _, c := range cs {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		mc := ManagedContainer{ID: c.ID, Name: name, Image: c.Image, State: c.State,
			Health: healthWord(c.Status), Labels: c.Labels, System: r.systemRole(ctx, c.ID, c.Labels)}
		if c.NetworkSettings != nil {
			for _, n := range c.NetworkSettings.Networks {
				if n.IPAddress != "" {
					mc.IPs = append(mc.IPs, n.IPAddress)
				}
			}
		}
		out = append(out, withTask(mc))
	}
	return out, nil
}

// ContainerDetail is a curated inspect view.
type ContainerDetail struct {
	ID       string
	Name     string
	Image    string
	State    string
	Started  string
	Ports    []string
	Mounts   []string
	Networks []string
}

// InspectContainer returns curated details for one container.
func (r *Runtime) InspectContainer(ctx context.Context, id string) (*ContainerDetail, error) {
	info, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &ContainerDetail{
		ID:    info.ID,
		Name:  strings.TrimPrefix(info.Name, "/"),
		Image: info.Config.Image,
	}
	if info.State != nil {
		d.State = info.State.Status
		d.Started = info.State.StartedAt
	}
	for p, bindings := range info.NetworkSettings.Ports {
		for _, b := range bindings {
			d.Ports = append(d.Ports, fmt.Sprintf("%s:%s -> %s", b.HostIP, b.HostPort, p))
		}
	}
	for _, m := range info.Mounts {
		d.Mounts = append(d.Mounts, fmt.Sprintf("%s -> %s", m.Source, m.Destination))
	}
	for name := range info.NetworkSettings.Networks {
		d.Networks = append(d.Networks, name)
	}
	return d, nil
}

// containerLogs opens the docker log stream for one container. The Go client
// rather than `docker logs`, so this works from inside the node agent's
// image (docs/plans/31-node-agent-open-questions.md).
func (r *Runtime) containerLogs(ctx context.Context, id string, tail int, follow, timestamps bool) (io.ReadCloser, error) {
	return r.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: follow,
		Timestamps: timestamps, Tail: strconv.Itoa(tail),
	})
}

// Logs returns the last tail lines of a container's logs, one-shot.
func (r *Runtime) Logs(ctx context.Context, id string, tail int) (string, error) {
	rc, err := r.containerLogs(ctx, id, tail, false, false)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	// One buffer for both streams: the caller shows this to a human and the
	// CLI interleaved them too.
	if _, err := stdcopy.StdCopy(&buf, &buf, rc); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

// StreamLogs follows container logs; returns a reader and a stop func.
func (r *Runtime) StreamLogs(ctx context.Context, id string, tail int) (io.Reader, func(), error) {
	rc, err := r.containerLogs(ctx, id, tail, true, false)
	if err != nil {
		return nil, nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		_ = pw.CloseWithError(err)
	}()
	stop := func() { _ = rc.Close(); _ = pr.Close() }
	return pr, stop, nil
}

// StreamLogsMarked follows container logs with docker timestamps, emitting
// one line per log entry prefixed "O " (stdout) or "E " (stderr) so viewers
// can colour the streams. The channel closes when the container stops or ctx
// ends; call stop to end early.
func (r *Runtime) StreamLogsMarked(ctx context.Context, id string, tail int) (<-chan string, func(), error) {
	rc, err := r.containerLogs(ctx, id, tail, true, true)
	if err != nil {
		return nil, nil, err
	}
	// stdcopy demuxes into two pipes so each stream keeps its own mark; the
	// CLI got the same split for free from two file descriptors.
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(outW, errW, rc)
		_ = outW.CloseWithError(err)
		_ = errW.CloseWithError(err)
	}()
	ch := make(chan string, 256)
	var wg sync.WaitGroup
	// Closing the read ends unblocks a StdCopy mid-Write and the sibling
	// pump mid-Scan; closing rc alone does not, and every closed log tab
	// used to leave three goroutines and two pipes behind.
	stop := func() { _ = rc.Close(); _ = outR.Close(); _ = errR.Close() }
	pump := func(rd io.Reader, mark string) {
		defer wg.Done()
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			select {
			case ch <- mark + " " + sc.Text():
			case <-ctx.Done():
				stop()
				return
			}
		}
	}
	wg.Add(2)
	go pump(outR, "O")
	go pump(errR, "E")
	go func() { wg.Wait(); stop(); close(ch) }()
	return ch, stop, nil
}

// streamMarked is the shared body of StreamLogsMarked and its service
// equivalent: `docker logs` and `docker service logs` differ only in the
// subcommand and in what the reference names.
func (r *Runtime) streamMarked(ctx context.Context, sub []string, tail int, ref string) (<-chan string, func(), error) {
	argv := append(append([]string{}, sub...), "--follow", "--timestamps", "--tail", fmt.Sprint(tail), ref)
	cmd := exec.CommandContext(ctx, "docker", argv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	out := make(chan string, 256)
	var wg sync.WaitGroup
	pump := func(rd io.Reader, mark string) {
		defer wg.Done()
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			select {
			case out <- mark + " " + sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdout, "O")
	go pump(stderr, "E")
	go func() { wg.Wait(); _ = cmd.Wait(); close(out) }()
	stop := func() { _ = cmd.Process.Kill() }
	return out, stop, nil
}

// VolumeInfo describes one docker named volume for the volumes UI.
type VolumeInfo struct {
	Name       string
	Driver     string
	Created    string
	Mountpoint string   // path on the host holding the data
	SizeBytes  int64    // -1 = unknown
	UsedBy     []string // names of *running* containers mounting it
	// HeldBy is the containers that mount it but are not running: a tile the
	// operator stopped, and the corpses swarm keeps in its task history after
	// a task is rescheduled to another node. Docker refuses to delete a
	// volume while either list is non-empty, so these have to be named rather
	// than folded into UsedBy: a volume held only by corpses is free to
	// delete, one held by a stopped tile is the tile's data.
	HeldBy []string
}

// InspectVolume returns one named volume with its host path and size. Size
// comes from docker's disk-usage scan (the only API that reports it) so this
// is a click-time call, not a pollable one.
func (r *Runtime) InspectVolume(ctx context.Context, name string) (VolumeInfo, error) {
	v, err := r.cli.VolumeInspect(ctx, name)
	if err != nil {
		return VolumeInfo{}, err
	}
	info := VolumeInfo{Name: v.Name, Driver: v.Driver, Created: v.CreatedAt, Mountpoint: v.Mountpoint, SizeBytes: -1}
	if du, err := r.cli.DiskUsage(ctx, types.DiskUsageOptions{Types: []types.DiskUsageObject{types.VolumeObject}}); err == nil {
		for _, dv := range du.Volumes {
			if dv.Name == name && dv.UsageData != nil {
				info.SizeBytes = dv.UsageData.Size
			}
		}
	}
	if cs, err := r.cli.ContainerList(ctx, container.ListOptions{All: true}); err == nil {
		for _, c := range cs {
			for _, m := range c.Mounts {
				if m.Type == "volume" && m.Name == name {
					cn := strings.TrimPrefix(c.Names[0], "/")
					if c.State == "running" {
						info.UsedBy = append(info.UsedBy, cn)
					} else {
						info.HeldBy = append(info.HeldBy, cn)
					}
				}
			}
		}
	}
	return info, nil
}

// ListVolumes returns all named volumes with sizes and the containers using
// them. Sizes come from docker's disk-usage scan (slower than a plain list,
// fine for a settings page, don't poll it).
func (r *Runtime) ListVolumes(ctx context.Context) ([]VolumeInfo, error) {
	vl, err := r.cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	if du, err := r.cli.DiskUsage(ctx, types.DiskUsageOptions{Types: []types.DiskUsageObject{types.VolumeObject}}); err == nil {
		for _, v := range du.Volumes {
			if v.UsageData != nil {
				sizes[v.Name] = v.UsageData.Size
			}
		}
	}
	used := map[string][]string{}
	held := map[string][]string{}
	if cs, err := r.cli.ContainerList(ctx, container.ListOptions{All: true}); err == nil {
		for _, c := range cs {
			name := strings.TrimPrefix(c.Names[0], "/")
			for _, m := range c.Mounts {
				if m.Type != "volume" {
					continue
				}
				if c.State == "running" {
					used[m.Name] = append(used[m.Name], name)
				} else {
					held[m.Name] = append(held[m.Name], name)
				}
			}
		}
	}
	out := make([]VolumeInfo, 0, len(vl.Volumes))
	for _, v := range vl.Volumes {
		size := int64(-1)
		if s, ok := sizes[v.Name]; ok {
			size = s
		}
		out = append(out, VolumeInfo{Name: v.Name, Driver: v.Driver, Created: v.CreatedAt, Mountpoint: v.Mountpoint, SizeBytes: size, UsedBy: used[v.Name], HeldBy: held[v.Name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FmtBytes renders a byte count for the UI ("1.2 GB", "340 MB").
func FmtBytes(n int64) string {
	switch {
	case n < 0:
		return "?"
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// CreateVolume creates a named docker volume (no-op if it exists).
func (r *Runtime) CreateVolume(ctx context.Context, name string) error {
	_, err := r.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name})
	return err
}

// RemoveVolume deletes a named volume. Fails while a container uses it.
// CreateVolumeOpts creates a named volume with an explicit driver + opts,
// how storage sub-paths become nfs/cifs/bind mounts. NOT idempotent across
// differing opts: docker returns an existing volume unchanged, so edits are
// remove-then-recreate.
func (r *Runtime) CreateVolumeOpts(ctx context.Context, name, driver string, opts map[string]string) error {
	_, err := r.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Driver: driver, DriverOpts: opts})
	return err
}

func (r *Runtime) RemoveVolume(ctx context.Context, name string) error {
	return r.cli.VolumeRemove(ctx, name, false)
}

// PurgeVolume removes the volume after clearing the stopped containers that
// hold it. Docker refuses to delete a volume any container references, and
// swarm keeps a rescheduled task's container in its history, so after a
// volume move the source copy was permanently undeletable: the node page
// offered no Delete button and the docs said to use it anyway.
//
// Only containers that are not running are removed, so this can never take
// down something that is serving. A running container still makes the whole
// call fail, from docker's own refusal, which is the safety net the panel
// relies on.
func (r *Runtime) PurgeVolume(ctx context.Context, name string) error {
	cs, err := r.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.State == "running" {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name == name {
				// Best effort: a container that vanished between the list and
				// here is one less thing holding the volume, and the
				// VolumeRemove below reports anything still in the way.
				_ = r.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
				break
			}
		}
	}
	return r.RemoveVolume(ctx, name)
}

// ContainerStats is one instantaneous reading for a running container.
// RxBytes/TxBytes are cumulative since container start (all interfaces).
type ContainerStats struct {
	CPUPct   float64
	MemBytes uint64
	RxBytes  uint64
	TxBytes  uint64
}

// Stats returns instantaneous CPU%/memory/network for a running container.
func (r *Runtime) Stats(ctx context.Context, id string) (ContainerStats, error) {
	var out ContainerStats
	resp, err := r.cli.ContainerStatsOneShot(ctx, id)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	var v container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return out, err
	}
	cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage - v.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(v.CPUStats.SystemUsage - v.PreCPUStats.SystemUsage)
	if sysDelta > 0 && cpuDelta >= 0 {
		out.CPUPct = cpuDelta / sysDelta * float64(v.CPUStats.OnlineCPUs) * 100
	}
	out.MemBytes = v.MemoryStats.Usage
	for _, n := range v.Networks {
		out.RxBytes += n.RxBytes
		out.TxBytes += n.TxBytes
	}
	return out, nil
}

// Prune removes stopped containers, dangling images, and build cache.
// Tagged images (incl. rollback targets) are untouched.
func (r *Runtime) Prune(ctx context.Context) (string, error) {
	// `docker system prune -f` is three API calls behind one command. The Go
	// client so the node agent can prune its own node without a docker CLI in
	// its image (docs/plans/30-docker-swarm.md, step 7).
	var reclaimed uint64
	var lines []string
	cr, err := r.cli.ContainersPrune(ctx, filters.Args{})
	if err != nil {
		return "", err
	}
	reclaimed += cr.SpaceReclaimed
	lines = append(lines, fmt.Sprintf("Deleted containers: %d", len(cr.ContainersDeleted)))

	// Dangling only, which is what prune without -a means: a tagged image is
	// somebody's rollback target.
	ir, err := r.cli.ImagesPrune(ctx, filters.NewArgs(filters.Arg("dangling", "true")))
	if err != nil {
		return strings.Join(lines, "\n"), err
	}
	reclaimed += ir.SpaceReclaimed
	lines = append(lines, fmt.Sprintf("Deleted images: %d", len(ir.ImagesDeleted)))

	br, err := r.cli.BuildCachePrune(ctx, build.CachePruneOptions{})
	if err != nil {
		return strings.Join(lines, "\n"), err
	}
	reclaimed += br.SpaceReclaimed
	lines = append(lines, "Total reclaimed space: "+FmtBytes(int64(reclaimed)))
	return strings.Join(lines, "\n"), nil
}

// ListByLabel lists containers (running or not) matching label=value.
func (r *Runtime) ListByLabel(ctx context.Context, label, value string) ([]ManagedContainer, error) {
	cs, err := r.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", label+"="+value)),
	})
	if err != nil {
		return nil, err
	}
	var out []ManagedContainer
	for _, c := range cs {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		out = append(out, withTask(ManagedContainer{
			ID: c.ID, Name: name, Image: c.Image, State: c.State, Labels: c.Labels}))
	}
	// Slot order, running first. Callers that take cs[0], logs, the terminal,
	// stats, must land on the same replica every time, and docker's own list
	// order is not stable across a rolling update.
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].State == "running") != (out[j].State == "running") {
			return out[i].State == "running"
		}
		return out[i].Slot < out[j].Slot
	})
	return out, nil
}

// ProxyRelay is one live forward-relay container.
type ProxyRelay struct {
	TileID string // label value: the tile it relays into
	Port   int    // target port, parsed from the container name suffix
}

// ListProxyRelays lists running relay containers across all tiles, the
// source of truth for the canvas's forward cards.
func (r *Runtime) ListProxyRelays(ctx context.Context) ([]ProxyRelay, error) {
	cs, err := r.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelProxyRelay)),
	})
	if err != nil {
		return nil, err
	}
	var out []ProxyRelay
	for _, c := range cs {
		name := ""
		if len(c.Names) > 0 {
			name = c.Names[0]
		}
		port, err := strconv.Atoi(name[strings.LastIndex(name, "-")+1:])
		if err != nil {
			continue // not one of ours, whatever it claims
		}
		out = append(out, ProxyRelay{TileID: c.Labels[LabelProxyRelay], Port: port})
	}
	return out, nil
}

// ListImageTags lists local image tags matching a repository reference
// (e.g. "stackr/<appid>"), returned as full "repo:tag" strings.
func (r *Runtime) ListImageTags(ctx context.Context, repository string) ([]string, error) {
	imgs, err := r.cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", repository)),
	})
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, im := range imgs {
		tags = append(tags, im.RepoTags...)
	}
	return tags, nil
}

// ListImageTagsByLabel lists the tags of every local image carrying
// label=value. Built images are labelled with the tile they belong to.
func (r *Runtime) ListImageTagsByLabel(ctx context.Context, label, value string) ([]string, error) {
	imgs, err := r.cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", label+"="+value)),
	})
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, im := range imgs {
		tags = append(tags, im.RepoTags...)
	}
	return tags, nil
}

// RemoveImage removes an image tag (no force; in-use images survive).
func (r *Runtime) RemoveImage(ctx context.Context, ref string) error {
	_, err := r.cli.ImageRemove(ctx, ref, image.RemoveOptions{})
	return err
}

// StopRemove force-removes a container.
func (r *Runtime) StopRemove(ctx context.Context, containerID string) error {
	timeout := 10
	_ = r.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
	return r.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
}

// RestartContainer restarts a container in place. Networks and addresses
// survive, unlike a recreate, so stored proxy addresses stay valid.
func (r *Runtime) RestartContainer(ctx context.Context, containerID string) error {
	timeout := 10
	return r.cli.ContainerRestart(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// StopContainer stops (but keeps) a container.
func (r *Runtime) StopContainer(ctx context.Context, containerID string) error {
	timeout := 10
	return r.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout})
}

// StartContainer starts an existing container.
func (r *Runtime) StartContainer(ctx context.Context, containerID string) error {
	return r.cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

// PauseContainer freezes every process in a container (SIGSTOP via cgroup).
// Files stop changing while it holds, which is what makes a tar of the
// container's volume a point-in-time copy rather than a torn one.
func (r *Runtime) PauseContainer(ctx context.Context, containerID string) error {
	return r.cli.ContainerPause(ctx, containerID)
}

// UnpauseContainer resumes a paused container. Safe to call on one that is not
// paused, a backup that failed mid-tar still ends with this.
func (r *Runtime) UnpauseContainer(ctx context.Context, containerID string) error {
	err := r.cli.ContainerUnpause(ctx, containerID)
	if err != nil && strings.Contains(err.Error(), "not paused") {
		return nil
	}
	return err
}

func portBindings(ports map[string]string) (nat.PortSet, nat.PortMap, error) {
	if len(ports) == 0 {
		return nil, nil, nil
	}
	exposed := nat.PortSet{}
	bindings := nat.PortMap{}
	for host, cont := range ports {
		if !strings.Contains(cont, "/") {
			cont += "/tcp"
		}
		p := nat.Port(cont)
		exposed[p] = struct{}{}
		bindings[p] = append(bindings[p], nat.PortBinding{HostIP: "0.0.0.0", HostPort: host})
	}
	return exposed, bindings, nil
}

// PullImageAuth and PushImageAuth move an image with a credential supplied per
// call, as the base64 auth blob the Docker API takes in its X-Registry-Auth
// header.
//
// Why not `docker login` first: the daemon config is shared by everything on
// the node, so logging in for one organization leaves its credential usable by
// the next organization's build. A per-call header keeps one org's token to one
// request. Blank auth is an anonymous pull, which is what a public image wants.
func (r *Runtime) PullImageAuth(ctx context.Context, ref, auth string, logW io.Writer) error {
	rc, err := r.cli.ImagePull(ctx, ref, image.PullOptions{RegistryAuth: auth})
	if err != nil {
		return err
	}
	_, err = drainProgress(rc, logW)
	return err
}

func (r *Runtime) PushImageAuth(ctx context.Context, ref, auth string, logW io.Writer) error {
	_, err := r.pushImage(ctx, ref, auth, logW)
	return err
}

// PushImageDigest pushes and returns the manifest digest the registry stored.
// Not the local image id: with the containerd image store a pulled multi-arch
// image's id is its index, while the push sends only this platform, so a ref
// pinned to the id names a manifest the registry never received.
func (r *Runtime) PushImageDigest(ctx context.Context, ref, auth string) (string, error) {
	digest, err := r.pushImage(ctx, ref, auth, io.Discard)
	if err == nil && digest == "" {
		err = fmt.Errorf("registry reported no digest for %s", ref)
	}
	return digest, err
}

func (r *Runtime) pushImage(ctx context.Context, ref, auth string, logW io.Writer) (string, error) {
	rc, err := r.cli.ImagePush(ctx, ref, image.PushOptions{RegistryAuth: auth})
	if err != nil {
		return "", err
	}
	return drainProgress(rc, logW)
}

// progressLine is the shape of one frame in docker's pull/push stream. Only the
// human-readable fields are read; the layer-by-layer progress bars are noise in
// a stored build log.
type progressLine struct {
	Status      string `json:"status"`
	Progress    string `json:"progress"`
	ID          string `json:"id"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// pushedDigest matches the line every push ends with, "<tag>: digest:
// sha256:... size: N". It is the one place both image stores report the
// manifest that actually went out: the containerd store sends no aux digest,
// and when it falls back from an index to one platform's manifest this line
// names the manifest.
var pushedDigest = regexp.MustCompile(`: digest: (sha256:[0-9a-f]{64}) size: `)

// drainProgress renders the JSON progress stream as plain lines and returns the
// error the stream reports. The stream's own error is the only signal there is:
// the HTTP call itself succeeds even when the push is refused, so a caller that
// only checked err would record a failed push as a finished deployment.
// Also returns the digest a push reports, "" for a pull.
func drainProgress(rc io.ReadCloser, logW io.Writer) (string, error) {
	defer func() { _ = rc.Close() }()
	dec := json.NewDecoder(rc)
	var streamErr error
	var digest string
	for {
		var l progressLine
		if err := dec.Decode(&l); err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
		if m := pushedDigest.FindStringSubmatch(l.Status); m != nil {
			digest = m[1]
		}
		if l.ErrorDetail != nil {
			streamErr = fmt.Errorf("%s", l.ErrorDetail.Message)
			continue
		}
		// Layer progress bars repeat per layer per tick; the status alone is
		// what a person reads back out of a log file.
		if l.Progress != "" {
			continue
		}
		if l.Status == "" {
			continue
		}
		if l.ID != "" {
			_, _ = fmt.Fprintf(logW, "%s: %s\n", l.ID, l.Status)
			continue
		}
		_, _ = fmt.Fprintln(logW, l.Status)
	}
	return digest, streamErr
}
