package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/FyrmForge/hamr/pkg/config"
	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/wgapply"
	"github.com/FyrmForge/stackr/internal/worker"
)

// runWorker is `stackrd worker`: the node program. The caller builds the
// mTLS config; the real command runner is filled in when none is given.
func runWorker(ctx context.Context, cfg worker.Config) error {
	if cfg.Runner == nil {
		cfg.Runner = worker.ExecRunner{}
	}
	if cfg.Mesh.WG == nil {
		cfg.Mesh.WG = kernelWG()
	}
	return worker.Run(ctx, cfg)
}

// workerConfig reads `stackrd worker`'s flags (each also an env var). The
// state dir holds the node's cert, key and the cluster CA cert, and the
// worker's saved state. The address is required: it is the node's wg
// address, and the worker never listens on every interface by default.
func workerConfig(args []string) (worker.Config, error) {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	state := fs.String("state-dir", config.GetEnvOrDefault("STACKR_WORKER_STATE", envDataDir), "node state dir (STACKR_WORKER_STATE)")
	addr := fs.String("addr", config.GetEnvOrDefault("STACKR_WORKER_ADDR", ""), "listen address on the wg address, host:port (STACKR_WORKER_ADDR)")
	cert := fs.String("cert", "", "node cert (default <state-dir>/node.crt)")
	key := fs.String("key", "", "node key (default <state-dir>/node.key)")
	ca := fs.String("ca", "", "cluster CA cert (default <state-dir>/cluster-ca.crt)")
	sock := fs.String("docker-sock", config.GetEnvOrDefault("STACKR_WORKER_DOCKER_SOCK", "/var/run/docker.sock"), "local Docker socket")
	caddy := fs.String("caddy-sock", config.GetEnvOrDefault("STACKR_WORKER_CADDY_SOCK", installspec.WorkerProxyAdminDir+"/admin.sock"),
		"Caddy admin socket of the proxy the controller starts on an ingress node (\"\" = off)")
	alive := fs.String("alive-addr", config.GetEnvOrDefault("STACKR_WORKER_ALIVE_ADDR", ":"+worker.AlivePort),
		"public liveness listener, answers a fixed line only (STACKR_WORKER_ALIVE_ADDR; \"\" = off)")
	renewNow := fs.Bool("renew-cert-now", config.GetEnvOrDefaultBool("STACKR_WORKER_RENEW_CERT_NOW", false),
		"renew the node cert at start even when not due (STACKR_WORKER_RENEW_CERT_NOW)")
	wgPort := fs.Int("wg-port", config.GetEnvOrDefaultInt("STACKR_WORKER_WG_PORT", mesh.DefaultPort),
		"UDP port of the mesh's WireGuard interface (STACKR_WORKER_WG_PORT)")
	panel := fs.String("panel-url", config.GetEnvOrDefault("STACKR_WORKER_PANEL_URL", ""),
		"the panel's public URL, where the worker recovers its mesh when the controller goes quiet (STACKR_WORKER_PANEL_URL; \"\" = the one the controller names)")
	if err := fs.Parse(args); err != nil {
		return worker.Config{}, err
	}
	if *addr == "" {
		return worker.Config{}, errors.New("--addr is required (the node's wg address and port)")
	}
	def := func(v *string, name string) string {
		if *v != "" {
			return *v
		}
		return filepath.Join(*state, name)
	}
	trust := worker.NewTrust(def(ca, "cluster-ca.crt"))
	tc, err := workerTLS(def(cert, "node.crt"), def(key, "node.key"), trust)
	if err != nil {
		return worker.Config{}, err
	}
	own, err := worker.LoadNodeCert(def(cert, "node.crt"), def(key, "node.key"))
	if err != nil {
		return worker.Config{}, fmt.Errorf("node cert: %w", err)
	}
	wg, err := workerWG()
	if err != nil {
		return worker.Config{}, err
	}
	return worker.Config{
		Version: version, AliveAddr: *alive, Recover: worker.RecoverConfig{Own: own, Panel: *panel},
		TLS: tc, Addr: *addr, DockerSock: *sock, CaddySock: *caddy, StateDir: *state, Trust: trust, RenewCertNow: *renewNow,
		Runner: worker.ExecRunner{}, Mesh: worker.MeshConfig{WG: wg, WGPort: *wgPort},
	}, nil
}

// workerWG is the kernel wg applier; STACKR_FAKE_HOST swaps in a recorder so
// a laptop worker never touches the host's interfaces.
func workerWG() (wgapply.Applier, error) {
	if config.GetEnvOrDefaultBool("STACKR_FAKE_HOST", false) {
		if !envDevMode {
			return nil, errors.New("STACKR_FAKE_HOST needs DEV_MODE=true: the fake host is for local development only")
		}
		return &wgapply.Recorder{}, nil
	}
	return wgapply.Kernel{}, nil
}

// workerTLS is the worker's listener config: its own cert, clients must
// hold a controller cert from the live trust list and, when the controller
// announced one, on its serial allow list. Both are read per handshake.
func workerTLS(certFile, keyFile string, trust *worker.Trust) (*tls.Config, error) {
	own, err := worker.LoadNodeCert(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("node cert: %w", err)
	}
	if len(trust.Roots()) == 0 {
		return nil, errors.New("cluster CA cert: none readable")
	}
	return clusterca.ServerConfig(trust.Roots, own, trust.Refuses), nil
}
