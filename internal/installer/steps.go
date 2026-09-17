package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// relayLocal is the tag runtime.ProxyRelayImage looks the relay up by, so the
// pulled image is retagged to match.
const relayLocal = "stkr-proxyrelay:local"

// runner runs the commands that change the box, or with dry prints them.
// Reads always run for real.
type runner struct {
	dry bool
	out io.Writer
}

// run executes a change. Its output is dropped unless show; on failure the
// command's stderr is the error.
func (r runner) run(ctx context.Context, show bool, name string, args ...string) error {
	if r.dry {
		_, _ = fmt.Fprintln(r.out, "  would run:", quoteCmd(name, args))
		return nil
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if show {
		cmd.Stdout, cmd.Stderr = r.out, io.MultiWriter(r.out, &stderr)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// read runs a command that changes nothing and returns its stdout.
func read(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// quoteCmd prints one flag per line, so a service create is readable.
func quoteCmd(name string, args []string) string {
	var b strings.Builder
	b.WriteString(name)
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			b.WriteString(" \\\n     ")
		}
		if strings.ContainsAny(a, " '\"") {
			a = "'" + a + "'"
		}
		b.WriteString(" " + a)
	}
	return b.String()
}

// Images is what the install pulls and runs.
type Images struct {
	Panel, Relay string
}

// Install creates the panel on this box from checked answers.
func Install(ctx context.Context, a *Answers, img Images, dry bool, out io.Writer) error {
	r := runner{dry: dry, out: out}

	if err := swarmInit(ctx, r); err != nil {
		return err
	}
	// Before anything is created: this restarts dockerd.
	if err := trustOwnRegistry(ctx, r, advertiseAddr()); err != nil {
		return err
	}
	if err := r.run(ctx, false, "install", "-d", "-m", "755", a.DataDir); err != nil {
		return err
	}
	if err := ensureNetwork(ctx, r); err != nil {
		return err
	}

	_, _ = fmt.Fprintln(out, "\n--- pulling images ---")
	for _, ref := range []string{img.Panel, img.Relay} {
		// A release tag never changes, so an image already here is the one
		// that would be pulled. Also what lets a box without registry access
		// install images loaded by hand.
		if _, err := read(ctx, "docker", "image", "inspect", ref); err == nil {
			_, _ = fmt.Fprintln(out, "  have", ref)
			continue
		}
		if err := r.run(ctx, true, "docker", "pull", ref); err != nil {
			return err
		}
	}
	if img.Relay != relayLocal {
		if err := r.run(ctx, false, "docker", "tag", img.Relay, relayLocal); err != nil {
			return err
		}
	}

	_, _ = fmt.Fprintln(out, "--- starting ---")
	if err := r.run(ctx, false, "docker", serviceArgs(a, img.Panel)...); err != nil {
		return err
	}
	return writeWrapper(r)
}

// serviceArgs is the panel's `docker service create`. The panel is a swarm
// service like everything else stackr runs, pinned to the manager: it holds
// the docker socket and the data dir, both this machine's.
//
// It publishes no port: reaching it is traefik's job, and when traefik or DNS
// is broken the way in is ssh plus the `stackr` CLI. Swarm published ports
// have no host IP anyway, so a loopback-only 127.0.0.1:8080 cannot exist on a
// service.
func serviceArgs(a *Answers, image string) []string {
	args := []string{"service", "create",
		"--name", "stackr",
		"--network", "stkr",
		"--hostname", "stkr-panel",
		"--constraint", "node.role == manager",
		"--replicas", "1",
	}
	tls, trustCF := "off", ""
	if a.TLS() {
		tls = "on"
	}
	if a.Cloudflare {
		trustCF = "1"
	}
	args = append(args,
		"--mount", "type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock",
		"--mount", "type=bind,src=/proc,dst=/host/proc,ro",
		"--mount", "type=bind,src="+a.DataDir+",dst="+a.DataDir,
	)
	env := [][2]string{
		{"BASE_URL", a.BaseURL()},
		{"ACME_EMAIL", a.Email},
		{"STACKR_TLS", tls},
		{"ROOT_DOMAIN", a.Root},
		{"TRUST_CLOUDFLARE", trustCF},
		{"TRUSTED_PROXY_CIDRS", a.Proxies},
		{"DATA_DIR", a.DataDir},
		{"DATABASE_PATH", a.DataDir + "/stackr.db"},
		{"TRAEFIK_HTTP_PORT", a.HTTPPort},
		{"HOST_PROC", "/host/proc"},
		{"STACKR_IMAGE", image},
	}
	// With HTTPS off there is no HTTPS port; traefik publishes none.
	if a.HTTPSPort != "" {
		env = append(env, [2]string{"TRAEFIK_HTTPS_PORT", a.HTTPSPort})
	}
	for _, kv := range env {
		args = append(args, "--env", kv[0]+"="+kv[1])
	}
	return append(args, "--stop-grace-period", "120s", image)
}

// Every stackr install is a Swarm, one node or many
// (docs/plans/30-docker-swarm.md). The overlay address pool is only settable
// at init, so it is picked carefully. The default 10.0.0.0/8 collides with too
// many LANs, and anything inside 172.16.0.0/12 collides with docker's own
// bridge pool.
var poolCandidates = []string{"10.250.0.0/15", "10.252.0.0/15", "10.254.0.0/15"}

func swarmInit(ctx context.Context, r runner) error {
	if v, _ := read(ctx, "docker", "info", "-f", "{{.Swarm.ControlAvailable}}"); v == "true" {
		_, _ = fmt.Fprintln(r.out, "  already a swarm manager; the address pool is fixed at init and stays as it is")
		return nil
	}
	pool, err := pickPool(poolCandidates, usedSubnets(ctx))
	if err != nil {
		return err
	}
	adv := advertiseAddr()
	if adv == "" {
		return errors.New("no address to advertise; set one up or run: docker swarm init --advertise-addr <ip>")
	}
	_, _ = fmt.Fprintf(r.out, "  swarm init on %s, overlay pool %s\n", adv, pool)
	return r.run(ctx, false, "docker", "swarm", "init", "--advertise-addr", adv,
		"--default-addr-pool", pool, "--default-addr-pool-mask-length", "24")
}

// pickPool returns the first candidate that overlaps nothing in used. Overlap
// goes both ways: an existing 10.0.0.0/8 route contains every candidate.
func pickPool(candidates, used []string) (string, error) {
	for _, c := range candidates {
		cp := netip.MustParsePrefix(c)
		clash := ""
		for _, u := range used {
			if up, err := netip.ParsePrefix(u); err == nil && up.Overlaps(cp) {
				clash = u
				break
			}
		}
		if clash == "" {
			return c, nil
		}
	}
	return "", fmt.Errorf("no free overlay address pool; every candidate (%s) overlaps a subnet this box already uses",
		strings.Join(candidates, " "))
}

// usedSubnets is every subnet this box routes to or has handed to a docker
// network.
func usedSubnets(ctx context.Context) []string {
	var used []string
	if routes, err := read(ctx, "ip", "-4", "route", "show"); err == nil {
		for _, l := range strings.Split(routes, "\n") {
			if f := strings.Fields(l); len(f) > 0 {
				used = append(used, f[0])
			}
		}
	}
	if ids, err := read(ctx, "docker", "network", "ls", "-q"); err == nil && ids != "" {
		args := append([]string{"network", "inspect", "-f", "{{range .IPAM.Config}}{{println .Subnet}}{{end}}"}, strings.Fields(ids)...)
		if subs, err := read(ctx, "docker", args...); err == nil {
			used = append(used, strings.Fields(subs)...)
		}
	}
	return used
}

// advertiseAddr is this box's private IPv4. The overlay data plane is plain
// VXLAN, so a private address is the one to advertise. Docker's own
// interfaces are skipped or docker0 wins the pick.
func advertiseAddr() string {
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if hasAnyPrefix(i.Name, "docker", "br-", "veth", "stkr") {
			continue
		}
		addrs, _ := i.Addrs()
		for _, ad := range addrs {
			if ipn, ok := ad.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsPrivate() {
				return ipn.IP.String()
			}
		}
	}
	// No private address: the one the default route leaves from. UDP dial
	// sends nothing.
	c, err := net.Dial("udp4", "1.1.1.1:80")
	if err != nil {
		return ""
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

const daemonJSON = "/etc/docker/daemon.json"

// trustOwnRegistry writes the insecure-registries entry the join script
// writes on every worker, for this box. The agent's service spec names the
// registry by this address, so the manager has to pull that ref too or its
// own agent task is rejected.
func trustOwnRegistry(ctx context.Context, r runner, addr string) error {
	port := os.Getenv("REGISTRY_PORT")
	if port == "" {
		port = "5000"
	}
	reg := addr + ":" + port
	cfg := map[string]any{}
	if b, err := os.ReadFile(daemonJSON); err == nil && len(bytes.TrimSpace(b)) > 0 {
		// Merge, never overwrite: the file holds the box's log driver, DNS,
		// data-root, whatever an operator put there.
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("%s is not valid JSON, so it cannot be merged. Add %q to its insecure-registries by hand and run this again", daemonJSON, reg)
		}
	}
	regs, _ := cfg["insecure-registries"].([]any)
	for _, x := range regs {
		if x == reg {
			return nil
		}
	}
	_, _ = fmt.Fprintln(r.out, "  trusting the panel's own registry at", reg)
	if r.dry {
		_, _ = fmt.Fprintf(r.out, "  would add %s to insecure-registries in %s and restart docker\n", reg, daemonJSON)
		return nil
	}
	cfg["insecure-registries"] = append(regs, reg)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll("/etc/docker", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(daemonJSON, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if r.run(ctx, false, "systemctl", "restart", "docker") != nil &&
		r.run(ctx, false, "service", "docker", "restart") != nil {
		return fmt.Errorf("could not restart docker to pick up %s; restart it and run this again", daemonJSON)
	}
	// dockerd takes a moment to come back and everything after needs it.
	for range 30 {
		if _, err := read(ctx, "docker", "info"); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return errors.New("docker did not come back after the restart")
}

// ensureNetwork creates the stkr overlay. Every stackr piece is a swarm
// service, and a service cannot join a bridge network; attachable because the
// forward relays are still plain containers.
//
// An older install has stkr as a bridge, and a network's driver cannot be
// changed. Carrying on would give a panel that looks healthy with no proxy,
// so this refuses.
func ensureNetwork(ctx context.Context, r runner) error {
	driver, err := read(ctx, "docker", "network", "inspect", "-f", "{{.Driver}}", "stkr")
	switch {
	case err != nil:
		// Plain VXLAN: join nodes over a VPN or a private network
		// (docs/plans/31-node-agent-open-questions.md, overlay encryption).
		return r.run(ctx, false, "docker", "network", "create", "-d", "overlay", "--attachable", "stkr")
	case driver != "overlay":
		return fmt.Errorf(`stkr already exists as a %q network and swarm services cannot join it.
Docker cannot change a network's driver, so it has to be recreated:

  docker service rm stackr
  docker network rm stkr
  # then run the installer again

Everything on it is recreated by the panel at boot; no data is on the network`, driver)
	}
	return nil
}

const wrapperPath = "/usr/local/bin/stackr"

// writeWrapper drops the admin CLI wrapper: with a domain the panel publishes
// no port, so when traefik or DNS is broken the way in is the CLI inside the
// container, where the panel is always localhost:8080.
func writeWrapper(r runner) error {
	if r.dry {
		_, _ = fmt.Fprintln(r.out, "  would write the stackr CLI wrapper to", wrapperPath)
		return nil
	}
	return os.WriteFile(wrapperPath, []byte(`#!/bin/sh
# stackr admin CLI: runs inside the panel container on this manager.
exec docker exec -i "$(docker ps -q -f label=com.docker.swarm.service.name=stackr | head -1)" /app/stackr "$@"
`), 0o755)
}

// Preflight refuses a box that cannot be installed on. Skipped in a dry run,
// which is usually on another machine.
func Preflight(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("run as root (docker, /var/lib and port 80 all need it)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker is not installed; https://docs.docker.com/engine/install/")
	}
	if _, err := read(ctx, "docker", "info"); err != nil {
		return errors.New("cannot talk to the docker daemon; is it running?")
	}
	// Rebuilding a live install would drop whatever the panel has changed on
	// its own service since.
	if _, err := read(ctx, "docker", "service", "inspect", "stackr"); err == nil {
		return errors.New(`stackr is already installed on this host.

Upgrade from the panel: Admin, Update.

If the panel will not start, move it to a release by hand:

  docker pull ghcr.io/fyrmforge/stackr-proxyrelay:<version>
  docker tag ghcr.io/fyrmforge/stackr-proxyrelay:<version> stkr-proxyrelay:local
  docker service update --image ghcr.io/fyrmforge/stackr:<version> \
    --env-add STACKR_IMAGE=ghcr.io/fyrmforge/stackr:<version> stackr`)
	}
	return nil
}

// portBusy reports whether something on this box listens on port.
func portBusy(port string) bool {
	// Not root in a dry run a low port cannot be bound whether it is taken or
	// not, so an answer on it is tried first.
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second); err == nil {
		_ = c.Close()
		return true
	}
	l, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return !errors.Is(err, syscall.EACCES)
	}
	_ = l.Close()
	return false
}
