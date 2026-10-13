package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/worker"
)

const (
	joinWorkerPort = "7443" // service.WorkerPort: where the head dials the worker
	joinBinary     = "/usr/local/bin/stackrd"
	joinUnitPath   = "/etc/systemd/system/stackr-worker.service"
)

// joinStatus is the part of the panel's join answer the installer reads.
type joinStatus struct {
	Status    string `json:"status"`
	NodeID    string `json:"node_id"`
	WGAddress string `json:"wg_address"`
	CertPEM   string `json:"cert_pem"`
	CAPEM     string `json:"ca_pem"`
	// Mesh is the node's first mesh spec; the worker brings its tunnel up
	// from it, so the controller never dials the node outside the mesh.
	Mesh json.RawMessage `json:"mesh"`
}

// joinCall is one trip to the panel: the request, the polling, and the
// check that the CA the panel hands back is the one the command named.
type joinCall struct {
	panel  string // https://host, no path
	token  string
	caSHA  string // hex, lower case
	client *http.Client
	poll   time.Duration
	say    func(...any)
}

// fingerprint is the SHA-256 of a CA cert, hex, as the panel prints it.
func fingerprint(pemBytes []byte) (string, error) {
	c, err := clusterca.ParseCertPEM(pemBytes)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(c.Raw)
	return hex.EncodeToString(h[:]), nil
}

// normalizeFP accepts the fingerprint as printed, with or without colons.
func normalizeFP(s string) (string, error) {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
	if b, err := hex.DecodeString(s); err != nil || len(b) != sha256.Size {
		return "", errors.New("--ca-sha256 must be the 64 hex characters of the SHA-256 the panel printed")
	}
	return s, nil
}

// errBusy is the panel turning a call away for its rate limit.
var errBusy = errors.New("panel busy")

func (j joinCall) post(ctx context.Context, path string, in any) (joinStatus, error) {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.panel+"/api/v1"+path, bytes.NewReader(body))
	if err != nil {
		return joinStatus{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := j.client.Do(req)
	if err != nil {
		return joinStatus{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusTooManyRequests {
		return joinStatus{}, errBusy
	}
	if resp.StatusCode/100 != 2 {
		return joinStatus{}, fmt.Errorf("panel answered %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var st joinStatus
	return st, json.Unmarshal(b, &st)
}

// run posts the request and waits for an admin. A confirmed answer is
// accepted only when its CA matches the fingerprint and signed the cert.
func (j joinCall) run(ctx context.Context, csrPEM []byte, wgPub string, facts any) (joinStatus, error) {
	st, err := j.post(ctx, "/join", map[string]any{
		"token": j.token, "csr": string(csrPEM), "wg_public_key": wgPub, "facts": facts,
	})
	if err != nil {
		return st, fmt.Errorf("join request: %w", err)
	}
	j.say("Waiting for an admin to confirm this node in the panel (Nodes, pending)...")
	for st.Status != "confirmed" || len(st.Mesh) == 0 || string(st.Mesh) == "null" {
		if st.Status == "rejected" {
			return st, errors.New("the admin rejected this node")
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(j.poll):
		}
		next, err := j.post(ctx, "/join/status", map[string]string{"token": j.token})
		if errors.Is(err, errBusy) {
			continue // the panel's rate limit: keep waiting
		}
		if err != nil {
			return st, fmt.Errorf("join status: %w", err)
		}
		st = next
	}
	got, err := fingerprint([]byte(st.CAPEM))
	if err != nil {
		return st, fmt.Errorf("the panel's CA cert: %w", err)
	}
	if got != j.caSHA {
		return st, fmt.Errorf("the panel's cluster CA is %s, not the %s the command named: refusing to trust it", got, j.caSHA)
	}
	ca, _ := clusterca.ParseCertPEM([]byte(st.CAPEM))
	cert, err := clusterca.ParseCertPEM([]byte(st.CertPEM))
	if err != nil {
		return st, fmt.Errorf("the node cert: %w", err)
	}
	if err := cert.CheckSignatureFrom(ca); err != nil {
		return st, fmt.Errorf("the node cert is not signed by that CA: %w", err)
	}
	if st.WGAddress == "" {
		return st, errors.New("the panel gave this node no address")
	}
	return st, nil
}

// workerUnit is the systemd unit for `stackrd worker`. The worker listens on
// its tunnel address only (bound before stackr0 has it, see worker.Run); it
// brings the tunnel up from its saved mesh spec at start, the first one from
// the join answer. --panel-url is where it recovers that spec when the
// controller goes quiet. After=docker.service keeps DOCKER-USER there.
func workerUnit(stateDir, wgAddr string, wgPort int, panel string) string {
	return fmt.Sprintf(`[Unit]
Description=stackr worker
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
ExecStart=%[1]s worker --state-dir %[2]s --addr %[4]s:%[3]s --wg-port %[5]d --panel-url %[6]s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, joinBinary, stateDir, joinWorkerPort, wgAddr, wgPort, panel)
}

// join is `stackr-install join`: turn this machine into a worker node of
// the panel named by the command an admin copied.
func join(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("stackr-install join", flag.ContinueOnError)
	panel := fs.String("panel", "", "the panel's URL, https://... (required)")
	token := fs.String("token", "", "the join token (required)")
	caSHA := fs.String("ca-sha256", "", "SHA-256 of the cluster CA cert, as the panel printed (required)")
	stateDir := fs.String("state-dir", "/var/lib/stackr-worker", "where the worker keeps its certs and state")
	release := fs.String("version", "", "release to install (default: this installer's own)")
	dry := fs.Bool("dry-run", false, "print what would change, change nothing")
	wgPort := fs.Int("wg-port", mesh.DefaultPort, "UDP port of this node's mesh WireGuard")
	yes := fs.Bool("yes", false, "install Docker with Docker's install script without asking, when it is missing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkWGPort(*wgPort); err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimRight(*panel, "/"))
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "":
		return errors.New("--panel must be the panel's https URL, like https://stkr.example.com")
	case *token == "":
		return errors.New("--token is missing")
	}
	fp, err := normalizeFP(*caSHA)
	if err != nil {
		return err
	}
	ver, err := checkVersion(*release, version)
	if err != nil {
		return err
	}
	r := runner{dry: *dry, out: out}
	if !r.dry && os.Geteuid() != 0 {
		return errors.New("run as root: joining installs packages and a system service")
	}

	r.say("Joining", u.String(), "as a stackr worker")
	if err := r.ensureDocker(ctx, *yes); err != nil {
		return err
	}
	// A rejoin runs on a box that already is a worker: its own services
	// hold the ports, and its networks already sit in stackr's ranges.
	_, err = os.Stat(filepath.Join(*stateDir, "node.crt"))
	fresh := err != nil
	miss := requirements(gatherHost(ctx), true, fresh)
	if fresh && !r.dry {
		ps := fixedPorts(true, *wgPort)
		if meshUp() {
			ps = ps[1:]
		}
		miss = append(miss, busyPorts(ps, hostBusy)...)
	}
	if err := failRequirements(r, miss); err != nil {
		return err
	}
	if err := r.ensureWireGuard(ctx); err != nil {
		return err
	}
	r.conntrackAcct() // tile traffic on this node
	if err := r.mkdir(*stateDir, 0o700); err != nil {
		return err
	}
	if err := r.fetchWorker(ctx, installspec.Image(ver)); err != nil {
		return err
	}
	if r.dry {
		r.say("  would request to join, wait for confirmation, write node.crt, node.key and cluster-ca.crt to", *stateDir,
			"and start stackr-worker.service")
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
	if !fresh {
		if err := dropWGKeys(*stateDir); err != nil {
			return err
		}
	}
	wgPub, err := worker.EnsureWGKey(*stateDir) // the worker reads the same wg.key later
	if err != nil {
		return err
	}
	r.say("  made this node's keys")
	j := joinCall{panel: u.String(), token: *token, caSHA: fp, poll: 10 * time.Second, say: r.say,
		client: &http.Client{Timeout: 30 * time.Second}}
	st, err := j.run(ctx, csr, wgPub, map[string]any{"interfaces": worker.Interfaces(), "wg_port": *wgPort})
	if err != nil {
		return err
	}
	r.say("  confirmed: node", st.NodeID, "at", st.WGAddress)
	for name, f := range map[string]struct {
		b    []byte
		mode os.FileMode
	}{
		"node.key": {kb, 0o600}, "node.crt": {[]byte(st.CertPEM), 0o644}, "cluster-ca.crt": {[]byte(st.CAPEM), 0o644},
		worker.MeshFile: {st.Mesh, 0o600},
	} {
		if err := os.WriteFile(filepath.Join(*stateDir, name), f.b, f.mode); err != nil {
			return err
		}
	}
	var spec worker.MeshSpec
	if err := json.Unmarshal(st.Mesh, &spec); err != nil {
		return fmt.Errorf("the node's mesh spec: %w", err)
	}
	if err := worker.ResetTrust(filepath.Join(*stateDir, "cluster-ca.crt"), spec.TrustRoots, spec.ControllerSerials); err != nil {
		return err
	}
	if err := os.WriteFile(joinUnitPath, []byte(workerUnit(*stateDir, strings.Split(st.WGAddress, "/")[0], *wgPort, u.String())), 0o644); err != nil {
		return err
	}
	// restart, not enable --now: on a rejoin the old worker still runs, on
	// the old cert and trust.
	for _, c := range [][]string{{"daemon-reload"}, {"enable", "stackr-worker.service"}, {"restart", "stackr-worker.service"}} {
		if err := r.run(ctx, "systemctl", c...); err != nil {
			return err
		}
	}
	r.say("This node is a worker now; the panel shows it under Nodes.")
	return nil
}

// dropWGKeys removes the mesh and exit keys of a box that joined before, so
// a rejoin (after break glass, say) comes back on new WireGuard keys.
func dropWGKeys(stateDir string) error {
	for _, f := range []string{"wg.key", "exit.key", "wg.key.prev", "exit.key.prev"} {
		if err := os.Remove(filepath.Join(stateDir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// run runs one command on the box, its output going to the installer's.
func (r runner) run(ctx context.Context, name string, args ...string) error {
	if r.dry {
		r.say("  would run:", name, quote(args))
		return nil
	}
	r.say(" ", name, strings.Join(args[:min(2, len(args))], " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = r.out, r.out
	return cmd.Run()
}

func have(name string) bool { _, err := exec.LookPath(name); return err == nil }

// ensureWireGuard makes sure the kernel has WireGuard. The worker talks to it
// through netlink, so no userspace tools are needed. A container shares the
// host's kernel: there the module must already be loaded on the host.
func (r runner) ensureWireGuard(ctx context.Context) error {
	if _, err := os.Stat("/sys/module/wireguard"); err == nil {
		r.say("  have wireguard")
		return nil
	}
	if err := r.run(ctx, "modprobe", "wireguard"); err != nil {
		return fmt.Errorf("wireguard: the kernel has no module (in a container, load it on the host): %w", err)
	}
	return nil
}

// fetchWorker puts the release's stackrd on the host, taking it out of the
// panel image (the one place releases publish it).
func (r runner) fetchWorker(ctx context.Context, image string) error {
	const tmp = "stackr-worker-extract"
	if err := r.pull(ctx, image); err != nil {
		return err
	}
	_ = r.run(ctx, "docker", "rm", "-f", tmp)
	if err := r.docker(ctx, "create", "--name", tmp, image); err != nil {
		return err
	}
	err := r.docker(ctx, "cp", tmp+":/stackrd", joinBinary+".new")
	_ = r.run(ctx, "docker", "rm", "-f", tmp)
	if err != nil {
		return err
	}
	if r.dry {
		return nil
	}
	if err := os.Chmod(joinBinary+".new", 0o755); err != nil {
		return err
	}
	return os.Rename(joinBinary+".new", joinBinary) // rename: a running worker keeps its old file
}
