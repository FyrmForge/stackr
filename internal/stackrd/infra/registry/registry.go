// Package registry manages the Stackr-hosted Docker registry (registry:2 with
// htpasswd auth) and external registry records.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

const registryImage = "registry:2"

// ServiceName is the registry's swarm service, which is also the name it
// answers to on the stkr overlay.
const ServiceName = "stkr-registry"

// EnsureManaged creates (if needed) and starts the managed registry service.
// It is reachable at localhost:<port>, inside Docker's default insecure-local
// allowance, so no TLS is needed for same-host pushes. A TLS domain through
// traefik (proxy.WriteRegistry) is what lets a worker pull without an
// insecure-registry entry.
// realm is where a docker client is sent to exchange a credential for a token.
// It has to be an address the *client* can reach, which for a build on the
// manager is stackrd's own listen address.
// Registries owns the registry row and the credentials the panel issues to
// itself. service.RegistryService satisfies it; an interface because that
// package is built on this one.
type Registries interface {
	EnsureRow(ctx context.Context, r *repo.Registry) error
	MintSystemCredential(ctx context.Context, c *repo.OrgRegistryCredential) error
	RevokeSystemCredential(ctx context.Context, id string) error
}

func EnsureManaged(ctx context.Context, store repo.Store, regs Registries, rt *runtime.Runtime, signer *Signer, dataDir, port, baseURL string) (*repo.Registry, error) {
	realm := strings.TrimSuffix(baseURL, "/") + TokenPath
	reg, err := store.GetManagedRegistry(ctx)
	if err != nil {
		return nil, err
	}
	if reg == nil {
		pw := secrets.RandomHex(16)
		reg = &repo.Registry{
			ID:        uuid.New().String(),
			Name:      "stackr (managed)",
			URL:       "localhost:" + port,
			Username:  "stackr",
			Password:  pw,
			Managed:   true,
			CreatedAt: time.Now().UTC(),
		}
		if err := regs.EnsureRow(ctx, reg); err != nil {
			return nil, err
		}
	}

	authDir := filepath.Join(dataDir, "registry")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return nil, err
	}
	// The token signing keypair is loaded once by the caller and handed to
	// both consumers. Loading it here as well raced the web token route on a
	// fresh data dir: both found no key, both generated, and the endpoint
	// signed with one while the registry trusted the other cert.
	if signer == nil {
		return nil, fmt.Errorf("registry token keypair: no signer")
	}

	if err := rt.PullImage(ctx, registryImage, os.Stderr); err != nil {
		return nil, fmt.Errorf("pull registry: %w", err)
	}
	absAuth, err := filepath.Abs(authDir)
	if err != nil {
		return nil, err
	}
	// A box that ran the registry as a plain container before the swarm move
	// still holds the name and the port; the service cannot start until it is
	// gone. The data lives in the named volume, which both use.
	for _, c := range mustList(ctx, rt) {
		_ = rt.StopRemove(ctx, c.ID)
	}
	// Pinned: the registry's storage is a local volume, so its task must not
	// be scheduled onto a node with an empty one. Host-mode ports keep
	// localhost:<port> exact on the manager, which is where builds push from.
	if _, err := rt.EnsureService(ctx, runtime.ServiceSpec{
		Name:  ServiceName,
		Image: registryImage,
		Env: []string{
			// Token auth, not htpasswd: one htpasswd user for the cluster made
			// the per-org image path a naming convention rather than a
			// boundary. stackrd is the token server (handlers/web, /v2/token)
			// and scopes every token to one org's namespace.
			"REGISTRY_AUTH=token",
			"REGISTRY_AUTH_TOKEN_REALM=" + realm,
			"REGISTRY_AUTH_TOKEN_SERVICE=" + Service,
			"REGISTRY_AUTH_TOKEN_ISSUER=" + Issuer,
			"REGISTRY_AUTH_TOKEN_ROOTCERTBUNDLE=/auth/token.crt",
			// Needed for the garbage collector to reclaim anything: without it
			// a deleted tag leaves its blobs on disk forever.
			"REGISTRY_STORAGE_DELETE_ENABLED=true",
		},
		Labels: map[string]string{"stackr.registry": "true"},
		Networks: []runtime.NetAttach{{
			Name: runtime.NetworkName,
			// "registry" is the name traefik's generated route dials
			// (proxy.WriteRegistry); the service keeps answering to it across
			// a roll because the alias is in the spec, not on the container.
			Aliases: []string{"registry"},
		}},
		Mounts: []string{
			absAuth + ":/auth",
			"stackr-registry-data:/var/lib/registry",
		},
		Ports:       map[string]string{port: "5000"},
		HostPorts:   true,
		Pinned:      true,
		ManagerOnly: true,
	}); err != nil {
		return nil, err
	}
	return reg, nil
}

// mustList is the pre-swarm cleanup lookup: plain containers still carrying
// the registry label. A lookup failure means "nothing to clean up" rather than
// failing a boot the registry service can still complete.
func mustList(ctx context.Context, rt *runtime.Runtime) []runtime.ManagedContainer {
	cs, err := rt.ListByLabel(ctx, "stackr.registry", "true")
	if err != nil {
		return nil
	}
	var out []runtime.ManagedContainer
	for _, c := range cs {
		if c.TaskID == "" { // a swarm task's container is the service's own
			out = append(out, c)
		}
	}
	return out
}

// PullAddr is the registry address every node can reach, which is not the
// same as the one the manager pushes to.
//
// The managed registry's URL is localhost:<port>, and that is right for a
// push: it happens on the manager, and docker allows plain HTTP to localhost
// without an insecure-registries entry. It is wrong for anything a service
// spec references, "localhost" on a worker means the worker, which has no
// registry, and the task is rejected with "failed to resolve reference".
//
// So: the TLS domain when the registry has one (traefik routes it and every
// node just works), otherwise the manager's own advertise address with the
// same port, which is exactly what the join script writes into the node's
// daemon.json insecure-registries.
// PanelAddr is how the panel's own process reaches the registry: the service
// name on the stkr overlay, resolved by docker's DNS.
//
// Not reg.URL. That is localhost:<port>, which is exact on the manager's host,
// because the registry publishes its port in host mode, and wrong inside the
// panel's own container, where localhost is the panel. The panel is a swarm
// service like everything else stackr runs, so its catalog and manifest reads
// have to go over the overlay.
func PanelAddr(reg *repo.Registry) string {
	port := "5000"
	if _, p, ok := strings.Cut(reg.URL, ":"); ok && p != "" {
		port = p
	}
	return ServiceName + ":" + port
}

func PullAddr(ctx context.Context, rt *runtime.Runtime, reg *repo.Registry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("no registry")
	}
	if reg.Domain != "" {
		return reg.Domain, nil
	}
	_, port, ok := strings.Cut(reg.URL, ":")
	if !ok {
		return "", fmt.Errorf("registry url %q has no port", reg.URL)
	}
	_, managerAddr, err := rt.JoinToken(ctx)
	if err != nil {
		return "", err
	}
	if managerAddr == "" {
		return "", fmt.Errorf("cannot tell this manager's own address")
	}
	return managerAddr + ":" + port, nil
}

// EncodeAuth builds the base64 credential blob swarm passes to every node so
// it can pull the image itself (the API's --with-registry-auth). Without it a
// task placed on a worker pulls anonymously and a private registry refuses it,
// on the manager only because the daemon there is already logged in.
//
// host is the pull address, not the push one: it is what the node dials.
func EncodeAuth(reg *repo.Registry, host string) string {
	if reg == nil || reg.Username == "" {
		return ""
	}
	b, err := json.Marshal(map[string]string{
		"username":      reg.Username,
		"password":      reg.Password,
		"serveraddress": host,
	})
	if err != nil {
		return ""
	}
	return base64.URLEncoding.EncodeToString(b)
}
