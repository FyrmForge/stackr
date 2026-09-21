package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

// Ensure creates the shared runtime key and the global agent service if they
// are not there yet, and makes sure the image it runs is one every node can
// actually pull.
//
// Called on the first Add node submit, before the node joins (plan 32): the
// service exists first so swarm places a task on the new node the moment it
// arrives. Swarm also places one on the manager, which the panel ignores,
// on its own node it uses the socket directly. Also called at boot when the
// service has gone missing, and after the encryption flip recreates the
// networks.
func Ensure(ctx context.Context, regs registry.Registries, rt *runtime.Runtime, dataDir string) error {
	local := PanelImage(ctx, rt)
	if local == "" {
		return fmt.Errorf("cannot tell which image the panel is running, so cannot start the node agent from it")
	}
	image, auth, err := publish(ctx, regs, rt, local)
	if err != nil {
		return err
	}
	// Both sides of a volume move run this image, and so does every volume op
	// the panel runs on its own node. Both, not just the move image: the agent
	// sets the pair for its process in cmd/stackrd/agent.go and the panel set
	// only one, so a manager that had never pulled alpine answered every local
	// volume size, browse and listing with "No such image: alpine:3", a
	// freshly installed single-node box, in other words.
	//
	// It ships rsync, it is already on every node, and it is the one image the
	// panel can be sure of.
	runtime.MoveImage = image
	runtime.VolumeToolImage = image

	if _, err := ensureKey(ctx, rt, dataDir); err != nil {
		return err
	}
	secretID, err := rt.SecretID(ctx, SecretName)
	if err != nil {
		return err
	}
	// The overlay's subnet, so the agent can pick the right interface instead
	// of the first private address it meets, which on a node running plain
	// containers is as likely to be docker_gwbridge. The panel already holds
	// this inspect and the spec already carries env, so the agent needs no
	// docker call of its own in front of its listener. Asking the agent's own
	// daemon would add a round trip and a worker-local view of an overlay;
	// trusting "the task's eth0" breaks the day the agent joins a second
	// network. An empty CIDR makes the agent refuse to listen, which is the
	// point (agent.overlayAddr).
	_, overlayCIDR, err := rt.NetworkMemberAddr(ctx, runtime.NetworkName, "")
	if err != nil {
		return fmt.Errorf("reading the %s subnet for the node agent: %w", runtime.NetworkName, err)
	}

	_, err = rt.EnsureService(ctx, runtime.ServiceSpec{
		Name:  ServiceName,
		Image: image,
		// Same binary, different entrypoint. One image for panel and agent is
		// what makes the upgrade path "update the agent service, then the
		// panel" (docs/plans/30-docker-swarm.md, step 7).
		Cmd:    []string{"agent"},
		Global: true,
		// The agent proxies its own node's socket. This is root on that node
		// by any other name, which is why the agent is a narrow set of named
		// calls and never a socket proxy.
		Mounts: []string{
			"/var/run/docker.sock:/var/run/docker.sock",
			// The sampler reads the host's /proc, not the container's.
			"/proc:/host/proc:ro",
			"/etc/os-release:/host/etc/os-release:ro",
		},
		Env: []string{
			"HOST_PROC=/host/proc",
			"STACKR_PANEL_URL=http://" + runtime.PanelAlias + ":8080",
			"STACKR_OVERLAY_CIDR=" + overlayCIDR,
		},
		Networks: []runtime.NetAttach{{Name: runtime.NetworkName}},
		Secrets: []runtime.SecretMount{{
			ID: secretID, Name: SecretName, Target: "agent_key",
		}},
		Labels:       map[string]string{LabelAgent: "true"},
		RegistryAuth: auth,
	})
	if err != nil {
		return err
	}

	// The panel has to be able to read the same key or every call it makes to
	// a remote agent fails with "no node agent on this install yet". Normally
	// ensureKey has just written it next to the database and there is nothing
	// to do. This is the fallback for a swarm whose secret already existed
	// with no file beside it, an upgrade from a build before the file, or a
	// data dir that was replaced. It restarts the panel, which is why it is
	// last and why it is not the usual path.
	if ReadKey(dataDir) != "" {
		return nil
	}
	self := rt.SelfServiceName(ctx)
	if self == "" {
		return nil
	}
	added, err := rt.MountSecret(ctx, self, secretID, SecretName, "agent_key")
	if err != nil {
		return fmt.Errorf("mounting the runtime key into the panel: %w", err)
	}
	if added {
		slog.Info("mounted the node agent key into the panel, restarting", "service", self)
	}
	return nil
}

// publish pushes the panel's own image into the managed registry and returns
// the reference every node can pull, plus the auth blob swarm needs to do it.
//
// This is the same finding as the tiles': a locally built tag exists on one
// machine, and a global service referencing it never places on any other node
// (docs/plans/30-docker-swarm.md, decision 3). A worker reaches the registry
// either through its TLS domain or through the insecure-registries entry the
// join script writes, which is why the join script writes one at all.
//
// An install with no registry yet keeps the local tag. That is correct on a
// single node and is caught on the second, when the agent task fails to
// place with an image-pull error naming the tag.
func publish(ctx context.Context, regs registry.Registries, rt *runtime.Runtime, local string) (image, auth string, err error) {
	// A release image is already on a public registry every node can reach,
	// as a multi-arch index, so each node pulls its own platform. Its tag
	// moves on every upgrade, so the spec changes and the agents roll. No
	// push, which is also no race against the panel's own token endpoint at
	// boot (docs/plans/44-agent-image-published.md).
	//
	// The auth is an explicit anonymous one, not "": an empty auth makes swarm
	// keep the one already in the spec, which on a service that used to point
	// at the managed registry is that registry's agent login, and every node
	// would send it to ghcr.
	if releaseImage.MatchString(local) {
		return local, anonymousAuth, nil
	}
	reg, err := regs.ManagedOrNil(ctx)
	if err != nil || reg == nil {
		return local, "", nil //nolint:nilerr // no registry is a single-node install, not a failure
	}
	// Two addresses for one registry, and they are not interchangeable.
	//
	// Push and login go to reg.URL, which is localhost:<port>: the only name
	// the manager's own daemon trusts without an insecure-registries entry,
	// and on a no-TLS install it has none for its own IP, a login to the IP
	// comes back "server gave HTTP response to HTTPS client" and the agent
	// never starts.
	//
	// The service spec cannot say localhost: on a worker that is the worker,
	// and the task is rejected with "failed to resolve reference". It says
	// the pull address, the TLS domain, or the manager's advertise address
	// and port, which is what the join script writes into every worker's
	// daemon.json. Same registry, same repository, same digest; only the host
	// part differs.
	host, err := registry.PullAddr(ctx, rt, reg)
	if err != nil {
		return "", "", err
	}
	pushed := reg.URL + "/stkr-agent:latest"
	if err := rt.TagImage(ctx, local, pushed); err != nil {
		return "", "", err
	}
	// The root credential in the request's own auth header. Not `docker
	// login`: the daemon config is shared by everything on the node, and this
	// is the one credential that can reach every org's namespace.
	//
	// By digest, not by the tag. The tag is always :latest, so an upgraded
	// panel would produce a byte-identical service spec, swarm would find
	// nothing to update, and every node would keep running the old agent,
	// which the panel then refuses to talk to on the version header, with no
	// way left in the product to fix it. The digest is the one the registry
	// reports for the push, not the local image's: for a multi-arch image the
	// local id is the whole index and only this platform was pushed.
	digest, err := rt.PushImageDigest(ctx, pushed, registry.EncodeAuth(reg, reg.URL))
	if err != nil {
		return "", "", fmt.Errorf("pushing the agent image: %w", err)
	}
	// The spec's credential is the pull-only agent identity, not the root
	// pair: the spec is handed to every node and the agent is global, so the
	// root pair there is a permanent cluster-wide key to every org's images.
	// The push above keeps the root pair; it is the manager's own call.
	return host + "/stkr-agent@" + digest,
		registry.Auth(registry.AgentUser, registry.AgentSecret(reg.Password), host), nil
}

// releaseImage is a published stackr release, the only ref install.sh and the
// panel upgrade ever set.
var releaseImage = regexp.MustCompile(`^ghcr\.io/fyrmforge/stackr:[0-9]+\.[0-9]+\.[0-9]+(@sha256:[0-9a-f]+)?$`)

// anonymousAuth is base64("{}"), an auth blob with no credentials in it.
const anonymousAuth = "e30="

// ensureKey creates the shared runtime key as a swarm secret, once. Returns
// the key when it made one and "" when it was already there, a secret's
// value cannot be read back, which is the point of it.
//
// Rotation is create a new secret, update both services, remove the old one.
// Not automated: it is a deliberate act, not a schedule.
func ensureKey(ctx context.Context, rt *runtime.Runtime, dataDir string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := hex.EncodeToString(b)
	existed, err := rt.EnsureSecret(ctx, SecretName, []byte(key))
	if err != nil {
		return "", err
	}
	if existed {
		return "", nil
	}
	// Kept on the panel's own disk as well. The panel needs the same key to
	// call any agent, and the only other way to hand it a swarm secret is to
	// update its service, which restarts it, in the middle of the Add-node
	// request whose reply is the join script the operator is waiting for.
	// The data dir already holds the database and the master key, so this
	// adds no new class of secret to the box.
	if err := os.WriteFile(KeyFile(dataDir), []byte(key), 0o600); err != nil {
		return "", fmt.Errorf("saving the runtime key for the panel: %w", err)
	}
	return key, nil
}

// KeyFile is where the panel keeps its copy of the runtime key.
func KeyFile(dataDir string) string { return filepath.Join(dataDir, "agent.key") }

// Running reports whether the agent service exists at all. Every "needs the
// node agent" state in the UI is this question.
func Running(ctx context.Context, rt *runtime.Runtime) bool {
	names, err := rt.ListServiceNames(ctx)
	if err != nil {
		return false
	}
	for _, n := range names {
		if n == ServiceName {
			return true
		}
	}
	return false
}

// PanelImage is the image the panel's own task runs, which is what the agent
// service is built from. Read from this container's own inspect, so an
// install running a local build gets that same local build on its nodes
// rather than a published tag nobody has.
func PanelImage(ctx context.Context, rt *runtime.Runtime) string {
	if v := os.Getenv("STACKR_IMAGE"); v != "" {
		return v
	}
	// Not the hostname: the panel runs with --hostname stkr-panel, which
	// inspects to nothing (runtime.SelfContainerID).
	self := runtime.SelfContainerID(ctx, rt)
	if self == "" {
		return ""
	}
	d, err := rt.InspectContainer(ctx, self)
	if err != nil || d == nil {
		slog.Debug("agent: cannot read the panel's own image", "error", err)
		return ""
	}
	// An image the panel was started from by digest is not a name any node
	// can pull; publish() re-tags it either way, but say so if there is no
	// registry to publish into.
	if strings.HasPrefix(d.Image, "sha256:") {
		slog.Warn("agent: the panel runs an image with no tag; nodes can only pull it once it is pushed to the registry")
	}
	return d.Image
}
