package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/FyrmForge/hamr/pkg/config"
	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/worker"
)

// runWorker is `stackrd worker`: the node program. The caller builds the
// mTLS config; the real command runner is filled in when none is given.
func runWorker(ctx context.Context, cfg worker.Config) error {
	if cfg.Runner == nil {
		cfg.Runner = worker.ExecRunner{}
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
	caddy := fs.String("caddy-sock", config.GetEnvOrDefault("STACKR_WORKER_CADDY_SOCK", ""), "local Caddy admin socket (\"\" = off)")
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
	tc, err := workerTLS(def(cert, "node.crt"), def(key, "node.key"), def(ca, "cluster-ca.crt"))
	if err != nil {
		return worker.Config{}, err
	}
	return worker.Config{TLS: tc, Addr: *addr, DockerSock: *sock, CaddySock: *caddy, StateDir: *state}, nil
}

// workerTLS is the worker's listener config: its own cert, clients must
// hold a controller cert from the CA. ponytail: no revoked check here, a
// worker has no store; revocation reaches it when certs are renewed or the
// controller pushes a list.
func workerTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	own, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("node cert: %w", err)
	}
	b, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("cluster CA cert: %w", err)
	}
	ca, err := clusterca.ParseCertPEM(b)
	if err != nil {
		return nil, fmt.Errorf("cluster CA cert: %w", err)
	}
	return clusterca.ServerConfig(ca, own, nil), nil
}
