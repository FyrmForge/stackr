package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Docker's default log cap: the same json-file cap every container stackr
// creates carries (internal/service/internal/docker logConfig). The daemon
// default covers what stackr cannot cap per container: the buildx builder
// (its docker-container driver refuses log options) and anything else on
// the box.

const daemonJSON = "/etc/docker/daemon.json"

var logOpts = map[string]string{"max-size": "10m", "max-file": "3"}

// mergeLogCap adds the log cap to a daemon.json's bytes (empty: no file),
// keeping every other key as it was. A log driver or cap the operator set
// differently is theirs: nothing changes and warn says so.
func mergeLogCap(old []byte) (out []byte, changed bool, warn string, err error) {
	cfg := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(old)) > 0 {
		if err := json.Unmarshal(old, &cfg); err != nil {
			return nil, false, "", fmt.Errorf("%s is not valid JSON: %w", daemonJSON, err)
		}
	}
	if raw, ok := cfg["log-driver"]; ok {
		var d string
		if json.Unmarshal(raw, &d) != nil || d != "json-file" {
			return nil, false, fmt.Sprintf("%s sets log-driver %s; left as is, stackr's log cap is not applied", daemonJSON, raw), nil
		}
	}
	opts := map[string]string{}
	if raw, ok := cfg["log-opts"]; ok {
		if err := json.Unmarshal(raw, &opts); err != nil {
			return nil, false, fmt.Sprintf("%s has log-opts stackr cannot read; left as is", daemonJSON), nil
		}
	}
	merged := maps.Clone(opts)
	for k, v := range logOpts {
		if have, ok := opts[k]; ok && have != v {
			return nil, false, fmt.Sprintf("%s sets log-opts %s=%s; left as is (stackr's default is %s)", daemonJSON, k, have, v), nil
		}
		merged[k] = v
	}
	if maps.Equal(merged, opts) { // json-file is Docker's default driver
		return old, false, "", nil
	}
	cfg["log-driver"], _ = json.Marshal("json-file")
	cfg["log-opts"], _ = json.Marshal(merged)
	out, err = json.MarshalIndent(cfg, "", "  ")
	return append(out, '\n'), true, "", err
}

// ensureLogCap puts the log cap into daemon.json and, when the file changed,
// restarts Docker (stackr's containers come back by their restart policy)
// and removes the buildx builder container, so the next build recreates it
// under the new default. A Docker that does not come back gets the old file
// again: dockerd refuses a key set both in the file and by a flag.
func (r runner) ensureLogCap(ctx context.Context) (bool, error) {
	changed, err := r.logCap(ctx, daemonJSON, func(ctx context.Context) error {
		if err := r.run(ctx, "systemctl", "restart", "docker"); err != nil {
			return err
		}
		return dockerUp(ctx, 60*time.Second)
	})
	if changed {
		ids, _ := read(ctx, "docker", "ps", "-aq", "--filter", builderName)
		for _, id := range strings.Fields(ids) {
			_ = r.docker(ctx, "rm", "-f", id)
		}
	}
	return changed, err
}

func (r runner) logCap(ctx context.Context, path string, restart func(context.Context) error) (bool, error) {
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	had := err == nil
	out, changed, warn, err := mergeLogCap(old)
	if err != nil {
		r.say("  warning:", err.Error()+"; Docker's log cap is not set")
		return false, nil
	}
	if warn != "" {
		r.say("  warning:", warn)
	}
	if !changed {
		return false, nil
	}
	if r.dry {
		r.say("  would set Docker's default log cap (json-file, 10m x 3) in", path, "and restart Docker")
		return false, nil
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := writeAtomic(path, out, mode); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	r.say("  set Docker's default log cap (json-file, 10m x 3) in", path+"; restarting Docker once")
	if err := restart(ctx); err != nil {
		if had {
			_ = writeAtomic(path, old, mode)
		} else {
			_ = os.Remove(path)
		}
		_ = restart(context.WithoutCancel(ctx))
		return false, fmt.Errorf("docker did not come back with the log cap, %s is put back: %w", path, err)
	}
	return true, nil
}

// dockerUp waits for the daemon to answer.
func dockerUp(ctx context.Context, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		if _, err := read(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("docker did not answer within %s", wait)
		case <-time.After(time.Second):
		}
	}
}

// writeAtomic writes b to path through a temp file in the same dir.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(f.Name(), mode); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// dockerLogCap is `stackr-install docker-logcap`: what a worker upgrade runs
// on the node once the new worker is committed (stackrd worker-swap starts
// it in a transient unit, which outlives the helper container a Docker
// restart kills).
func dockerLogCap(ctx context.Context, out io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("run as root")
	}
	_, err := runner{out: out}.ensureLogCap(ctx)
	return err
}
