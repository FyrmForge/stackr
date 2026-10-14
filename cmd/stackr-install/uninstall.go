package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/worker"
)

// uninstall is `stackr-install uninstall [--purge] [--yes]`: take stackr off
// a worker box. Volumes and images stay (a rejoin finds its data again);
// --purge removes those and the node's state too, once the node's name is
// typed.

const (
	managedLabel = "label=stackr.managed=true"
	// stackrChain is the prefix of every iptables chain stackr makes.
	stackrChain = "STACKR-"
	// moveComment marks the INPUT rules a volume move opens (worker.moveComment).
	moveComment = "stackr-move"
	// exitIfacePrefix starts every exit link's name (exits.Iface).
	exitIfacePrefix = "wgx-"
)

// uninstallEnv is the box and its effects: r runs (or, dry, prints) every
// change, read answers the queries that decide what to change.
type uninstallEnv struct {
	r          runner
	read       func(ctx context.Context, name string, args ...string) (string, error)
	in         io.Reader // the purge prompt's answer
	yes        bool
	stateDir   string
	installDir string
}

func uninstall(ctx context.Context, args []string, out io.Writer, in io.Reader) error {
	fs := flag.NewFlagSet("stackr-install uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also remove stackr's volumes and images and this node's state")
	yes := fs.Bool("yes", false, "do not ask before uninstalling (--purge still asks for the node's name)")
	dry := fs.Bool("dry-run", false, "print what would change, change nothing")
	stateDir := fs.String("state-dir", "/var/lib/stackr-worker", "where the worker keeps its certs and state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*dry && os.Geteuid() != 0 {
		return errors.New("run as root: uninstalling removes a system service, containers and firewall rules")
	}
	return uninstallEnv{
		r: runner{dry: *dry, out: out}, read: read, in: in, yes: *yes,
		stateDir: *stateDir, installDir: envOr("STACKR_DATA_DIR", "/var/lib/stackr"),
	}.run(ctx, *purge)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (e uninstallEnv) has(name string) bool {
	_, err := os.Stat(filepath.Join(e.stateDir, name))
	return err == nil
}

// controller: this box runs the panel (it has an install.json) and is no
// worker. Uninstall removes workers; a controller is a separate, rarer job.
func (e uninstallEnv) controller() bool {
	_, err := os.Stat(filepath.Join(e.installDir, installspec.File))
	return err == nil && !e.has("node.crt") && !e.has(worker.MeshFile) && !e.has(nodeNameFile)
}

func (e uninstallEnv) run(ctx context.Context, purge bool) error {
	if e.controller() {
		return errors.New("this box runs the panel; uninstall removes workers")
	}
	if purge {
		if err := checkStateDir(e.stateDir); err != nil {
			return err
		}
		if err := e.confirmPurge(); err != nil {
			return err
		}
	}
	r := e.r
	var errs []error
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	// try is for what may already be gone.
	try := func(name string, args ...string) { _ = r.run(ctx, name, args...) }

	// The service first, so nothing is made again while it comes apart.
	r.say("Removing the worker service")
	try("systemctl", "disable", "--now", workerUnitName)
	fail(r.run(ctx, "rm", "-f", joinUnitPath))
	try("systemctl", "daemon-reload")

	r.say("Removing stackr's containers and networks")
	e.removeAll(ctx, "docker", []string{"ps", "-aq", "--filter", managedLabel}, []string{"rm", "-f"}, fail)
	// The proxy and the host helper that ran this are named, not labelled managed.
	try("docker", "rm", "-f", installspec.ProxyName, "stackr-host-helper")
	e.removeAll(ctx, "docker", []string{"network", "ls", "-q", "--filter", managedLabel}, []string{"network", "rm"}, fail)

	r.say("Removing stackr's firewall rules")
	fail(e.iptables(ctx))

	r.say("Removing the mesh")
	e.mesh(ctx, try)

	if !purge {
		r.say("Done. Volumes, images and", e.stateDir, "stay; --purge removes them.")
		return errors.Join(errs...)
	}
	r.say("Purging volumes, images and state")
	e.removeAll(ctx, "docker", []string{"volume", "ls", "-q", "--filter", managedLabel}, []string{"volume", "rm"}, fail)
	try("docker", "volume", "rm", "stackr-caddy") // an ingress node's only
	e.removeImages(ctx, fail)
	fail(r.run(ctx, "rm", "-rf", e.stateDir))
	r.say("Done. stackr is off this box.")
	return errors.Join(errs...)
}

const workerUnitName = "stackr-worker.service"

// removeAll runs `docker <rm...> <ids>` for the ids a query lists (plus
// extra), when there are any.
func (e uninstallEnv) removeAll(ctx context.Context, bin string, query, rm []string, fail func(error), extra ...string) {
	out, err := e.read(ctx, bin, query...)
	if err != nil {
		fail(fmt.Errorf("%s %s: %w", bin, strings.Join(query[:min(2, len(query))], " "), err))
		return
	}
	ids := append(strings.Fields(out), extra...)
	if len(ids) > 0 {
		fail(e.r.run(ctx, bin, append(slices.Clone(rm), ids...)...))
	}
}

// removeImages removes stackr's images: the panel image by release,
// whatever carries the managed label, and every image pulled through the
// registry front (localhost:<port>/...: the system images at any depth and
// the orgs' tile images).
func (e uninstallEnv) removeImages(ctx context.Context, fail func(error)) {
	var ids []string
	add := func(id string) {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, f := range []string{managedLabel, "reference=" + installspec.Repo} {
		out, err := e.read(ctx, "docker", "image", "ls", "-q", "--filter", f)
		if err != nil {
			fail(fmt.Errorf("docker image ls: %w", err))
			continue
		}
		for _, id := range strings.Fields(out) {
			add(id)
		}
	}
	out, err := e.read(ctx, "docker", "image", "ls", "--format", "{{.Repository}} {{.ID}}")
	if err != nil {
		fail(fmt.Errorf("docker image ls: %w", err))
	}
	front := "localhost:" + installspec.RegistryPort + "/"
	for _, l := range strings.Split(out, "\n") {
		if repo, id, ok := strings.Cut(strings.TrimSpace(l), " "); ok && strings.HasPrefix(repo, front) {
			add(id)
		}
	}
	if len(ids) > 0 {
		fail(e.r.run(context.WithoutCancel(ctx), "docker", append([]string{"image", "rm", "-f"}, ids...)...))
	}
}

// iptables removes stackr's rules and chains with one restore --noflush, so
// it applies whole or not at all and touches nothing it was not told to.
func (e uninstallEnv) iptables(ctx context.Context) error {
	save, err := e.read(ctx, "iptables-save")
	if err != nil {
		return fmt.Errorf("iptables-save: %w", err)
	}
	payload := iptablesPurge(save)
	if payload == "" {
		e.r.say("  none there")
		return nil
	}
	return e.r.runIn(ctx, payload, "iptables-restore", "--noflush")
}

// mesh deletes stackr0, the exit links and their route tables and rules, by
// the names the worker saved when it applied its mesh.
func (e uninstallEnv) mesh(ctx context.Context, try func(string, ...string)) {
	var spec worker.MeshSpec
	if b, err := os.ReadFile(filepath.Join(e.stateDir, worker.MeshFile)); err == nil {
		if err := json.Unmarshal(b, &spec); err != nil {
			e.r.say("  the saved mesh state is unreadable; removing stackr0 only:", err)
			spec = worker.MeshSpec{}
		}
	}
	ifaces := []string{mesh.Iface}
	var tables []int
	for _, l := range spec.Links {
		ifaces, tables = append(ifaces, l.Iface), append(tables, l.Table)
	}
	for _, s := range spec.Serves {
		ifaces, tables = append(ifaces, s.Iface), append(tables, s.Table)
	}
	for _, i := range ifaces {
		try("ip", "link", "del", i)
	}
	for _, t := range tables {
		try("ip", "route", "flush", "table", strconv.Itoa(t))
	}
	for _, s := range spec.Serves {
		try("ip", "rule", "del", "pref", strconv.Itoa(s.Pref))
	}
	for _, l := range spec.Ladders {
		for _, st := range l.Subject.Clear() {
			try(st.Argv[0], st.Argv[1:]...)
		}
	}
}

// confirmPurge asks for the node's name. --yes never stands in for it: a
// purge deletes data a rejoin would have found again.
func (e uninstallEnv) confirmPurge() error {
	want, err := e.nodeName()
	if err != nil {
		return err
	}
	e.r.say("--purge removes stackr's volumes and images and", e.stateDir, "from this box.")
	e.r.say("Type the node's name to go on (the name the panel shows for it; an older node: its id):")
	line, _ := bufio.NewReader(e.in).ReadString('\n')
	if strings.TrimSpace(line) != want {
		return errors.New("that is not this node's name; nothing was removed")
	}
	return nil
}

// nodeName is what this box was joined as: node.name, else the id in
// node.crt's CN (boxes that joined before node.name existed).
func (e uninstallEnv) nodeName() (string, error) {
	if b, err := os.ReadFile(filepath.Join(e.stateDir, nodeNameFile)); err == nil {
		if n := strings.TrimSpace(string(b)); n != "" {
			return n, nil
		}
	}
	if b, err := os.ReadFile(filepath.Join(e.stateDir, "node.crt")); err == nil {
		if c, err := clusterca.ParseCertPEM(b); err == nil && c.Subject.CommonName != "" {
			return c.Subject.CommonName, nil
		}
	}
	return "", fmt.Errorf("cannot tell which node this is: no %s or node.crt in %s", nodeNameFile, e.stateDir)
}

// checkStateDir keeps rm -rf away from anything that is not a worker state
// dir's shape: absolute, clean, at least two levels deep.
func checkStateDir(d string) error {
	if !filepath.IsAbs(d) || filepath.Clean(d) != d || strings.Count(d, "/") < 2 {
		return fmt.Errorf("--state-dir %q is not a worker state directory", d)
	}
	return nil
}

// runIn is run with stdin, for iptables-restore.
func (r runner) runIn(ctx context.Context, stdin, name string, args ...string) error {
	if r.dry {
		r.say("  would run:", name, quote(args), "with:")
		for _, l := range strings.Split(strings.TrimSpace(stdin), "\n") {
			r.say("    " + l)
		}
		return nil
	}
	r.say(" ", name, strings.Join(args[:min(2, len(args))], " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(stdin), r.out, r.out
	return cmd.Run()
}

// iptablesPurge turns an iptables-save into an iptables-restore --noflush
// payload that deletes what stackr owns and nothing else: per table, the
// rules in other chains that jump to a STACKR- chain, carry the move comment
// or name an exit link; then every STACKR- chain flushed and deleted.
// "" when stackr has nothing there. Only the four tables stackr uses are read.
// ponytail: IPv4 only; stackr writes no ip6tables rules.
func iptablesPurge(save string) string {
	var out strings.Builder
	var table string
	var del, chains []string
	flush := func() {
		if len(del)+len(chains) == 0 {
			return
		}
		fmt.Fprintf(&out, "*%s\n", table)
		for _, d := range del {
			out.WriteString(d + "\n")
		}
		for _, c := range chains {
			out.WriteString("-F " + c + "\n")
		}
		for _, c := range chains {
			out.WriteString("-X " + c + "\n")
		}
		out.WriteString("COMMIT\n")
	}
	for _, line := range strings.Split(save, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "*"):
			table, del, chains = line[1:], nil, nil
		case line == "COMMIT":
			if slices.Contains([]string{"filter", "nat", "raw", "mangle"}, table) {
				flush()
			}
			table, del, chains = "", nil, nil
		case strings.HasPrefix(line, ":"):
			if name, _, _ := strings.Cut(line[1:], " "); strings.HasPrefix(name, stackrChain) {
				chains = append(chains, name)
			}
		case strings.HasPrefix(line, "-A "):
			chain, rule, _ := strings.Cut(line[3:], " ")
			if !strings.HasPrefix(chain, stackrChain) && ownedRule(rule) {
				del = append(del, "-D "+chain+" "+rule)
			}
		}
	}
	return out.String()
}

// ownedRule: a rule outside stackr's chains that is stackr's. The target
// (-j or -g) is a STACKR- chain, the comment is stackr's move comment, or the
// interface is an exit link.
func ownedRule(rule string) bool {
	f := strings.Fields(rule)
	for i, w := range f {
		if i+1 >= len(f) {
			break
		}
		v := strings.Trim(f[i+1], `"`)
		switch w {
		case "-j", "-g":
			if strings.HasPrefix(v, stackrChain) {
				return true
			}
		case "--comment":
			if v == moveComment {
				return true
			}
		case "-i", "-o":
			if strings.HasPrefix(v, exitIfacePrefix) {
				return true
			}
		}
	}
	return false
}
