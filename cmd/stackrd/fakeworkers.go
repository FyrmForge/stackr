package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/wgapply"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The fake host's remote nodes: real `worker` servers (mTLS, extras API,
// Docker pass-through) in this process, each on its own data/fakehost/<name>
// dir and a fixed localhost port, with the Recorder for WireGuard and a no-op
// command runner. They join the local panel through service.FakeJoinNode on
// first start and reuse their certs after, so the multi-node UI has nodes
// to show. Local development only (main refuses STACKR_FAKE_HOST without
// DEV_MODE). ponytail: the Docker behind them is a stub that answers info
// and empty lists, not the stateful fakehost Docker; tiles do not run on
// fake workers. Serving that over the Docker API is the upgrade.
var fakeNodes = []struct {
	name string
	tags []string
	port int
}{
	{"fake-1", []string{"ingress", "egress"}, 7451},
	{"fake-2", []string{"builder"}, 7452},
}

func startFakeWorkers(ctx context.Context, orch *service.Orchestrator, fakeDir string) error {
	for i, n := range fakeNodes {
		dir := filepath.Join(fakeDir, n.name)
		addr := fmt.Sprintf("127.0.0.1:%d", n.port)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := fakeJoin(ctx, orch, n.name, n.tags, addr, dir, i); err != nil {
			return fmt.Errorf("fake worker %s: %w", n.name, err)
		}
		sock, err := fakeDocker(ctx, dir)
		if err != nil {
			return err
		}
		tc, err := workerTLS(filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key"), filepath.Join(dir, "cluster-ca.crt"))
		if err != nil {
			return fmt.Errorf("fake worker %s: %w", n.name, err)
		}
		cfg := worker.Config{
			TLS: tc, Addr: addr, DockerSock: sock, StateDir: dir, Runner: noRunner{},
			Mesh: worker.MeshConfig{WG: &wgapply.Recorder{}},
		}
		go func() {
			if err := runWorker(ctx, cfg); err != nil && ctx.Err() == nil {
				slog.Error("fake worker stopped", "node", n.name, "err", err)
			}
		}()
		slog.Info("fake worker", "node", n.name, "addr", addr)
	}
	return nil
}

// fakeJoin makes the node's keys and joins it, unless its cert is already
// in dir from an earlier start.
func fakeJoin(ctx context.Context, orch *service.Orchestrator, name string, tags []string, addr, dir string, idx int) error {
	if _, err := os.Stat(filepath.Join(dir, "node.crt")); err == nil {
		return nil
	}
	key, err := clusterca.NewKey()
	if err != nil {
		return err
	}
	csr, err := clusterca.NewCSR(key)
	if err != nil {
		return err
	}
	kb, err := clusterca.KeyPEM(key)
	if err != nil {
		return err
	}
	wgPub, err := worker.EnsureWGKey(dir)
	if err != nil {
		return err
	}
	facts, _ := json.Marshal(map[string]any{"interfaces": []worker.Iface{
		{Name: "eth0", Prefixes: []string{fmt.Sprintf("192.168.%d.10/24", 60+idx)}},
	}})
	st, err := orch.FakeJoinNode(ctx, name, tags, addr, string(csr), wgPub, facts)
	if err != nil {
		return err
	}
	for f, b := range map[string][]byte{
		"node.key": kb, "node.crt": []byte(st.CertPEM), "cluster-ca.crt": []byte(st.CAPEM),
	} {
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

type noRunner struct{}

func (noRunner) Run(context.Context, []byte, string, ...string) ([]byte, error) { return nil, nil }

// fakeDocker serves the stub Docker API on dir/docker.sock.
func fakeDocker(ctx context.Context, dir string) (string, error) {
	sock := filepath.Join(dir, "docker.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"ServerVersion": "29.0.0", "DockerRootDir": dir})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/_ping":
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(p, "/json"), strings.HasSuffix(p, "/networks"):
			_, _ = w.Write([]byte("[]"))
		default:
			http.Error(w, `{"message":"not available on the fake host"}`, http.StatusNotFound)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("fake docker", "err", err)
		}
	}()
	go func() { <-ctx.Done(); _ = srv.Close() }()
	return sock, nil
}
