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
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/worker"
)

// uninstall is `stackr-install uninstall [--purge] [--yes]`: take stackr off
// a worker box. Volumes, images and binaries stay (a rejoin finds its data
// again); --purge removes those and the node's state too, once the node's
// name is typed. On the controller only --purge runs: it takes the panel and
// all its data off, once the panel's host name is typed.

const (
	managedLabel = "label=stackr.managed=true"
	// roleLabel is on every container stackr runs by role: the panel, proxy,
	// registry, observe, upgrader and host helper, and tiles' replicas.
	roleLabel = "label=" + installspec.LabelRole
	// imageLabel is on every image this repo builds (the two Dockerfiles,
	// the rsync helper), so one that lost its tag to a newer release is
	// still found.
	imageLabel = "label=stackr.image"
	// buildkitImage is what buildx pulls for the stackr-builder: its
	// docker-container driver's default, which no stackr code names.
	buildkitImage = "moby/buildkit:buildx-stable-1"
	// builderPrefix starts the buildx builder's container and state volume.
	builderPrefix = "buildx_buildkit_stackr-builder"
	builderName   = "name=^" + builderPrefix
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
	// formerNode: a join clears what a former node left before it goes on.
	// It also drops the proxy's certificates (another cluster's) and prints
	// no Done, since the join is not over.
	formerNode bool
}

func uninstall(ctx context.Context, args []string, out io.Writer, in io.Reader) error {
	fs := flag.NewFlagSet("stackr-install uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also remove stackr's volumes, images and binaries and this node's state")
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
	ctrl := e.controller()
	if ctrl && (!purge || e.formerNode) {
		return errors.New("this box runs the panel; uninstall --purge removes the panel and all its data")
	}
	// A panel whose install.json is elsewhere (another STACKR_DATA_DIR) would
	// go with the role-labelled containers below, unasked.
	if p, _ := e.read(ctx, "docker", "ps", "-aq", "--filter", roleLabel+"=panel"); !ctrl && strings.TrimSpace(p) != "" {
		return errors.New("this box runs the panel; set STACKR_DATA_DIR to its data dir and run uninstall --purge")
	}
	// A worker keeps its mesh state in the state dir; the controller's own
	// node keeps it under the data dir (service localMesh).
	meshDir, gone, host := e.stateDir, e.stateDir, ""
	if ctrl {
		in, err := installspec.Load(e.installDir)
		if err != nil {
			return fmt.Errorf("read the install: %w", err)
		}
		meshDir, gone, host = filepath.Join(e.installDir, "keys"), e.installDir, in.PanelHost
	}
	if purge {
		if err := checkStateDir(gone); err != nil {
			return err
		}
		if err := e.confirmPurge(ctrl, host, gone); err != nil {
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
	// -v takes the anonymous volumes an image's VOLUME lines gave a
	// container (Caddy's among them); named volumes stay.
	rmc := []string{"rm", "-f", "-v"}

	// The service first, so nothing is made again while it comes apart. On
	// the controller that is the panel: it re-applies rules and the mesh too.
	if ctrl {
		r.say("Removing the panel")
		e.removeAll(ctx, "docker", []string{"ps", "-aq", "--filter", roleLabel + "=panel"}, rmc, fail)
	} else {
		r.say("Removing the worker service")
		try("systemctl", "disable", "--now", workerUnitName)
		fail(r.run(ctx, "rm", "-f", joinUnitPath))
		try("systemctl", "daemon-reload")
	}

	r.say("Removing stackr's containers and networks")
	e.removeAll(ctx, "docker", []string{"ps", "-aq", "--filter", roleLabel}, rmc, fail)
	e.removeAll(ctx, "docker", []string{"ps", "-aq", "--filter", managedLabel}, rmc, fail)
	// buildx makes the builder's container, so it has no label; it sits on
	// stackr-builder-net and the network rm fails while it lives.
	e.removeAll(ctx, "docker", []string{"ps", "-aq", "--filter", builderName}, rmc, fail)
	e.removeAll(ctx, "docker", []string{"network", "ls", "-q", "--filter", managedLabel}, []string{"network", "rm"}, fail)

	r.say("Removing stackr's firewall rules")
	fail(e.iptables(ctx))

	r.say("Removing the mesh")
	e.mesh(ctx, meshDir, try)

	if e.formerNode {
		e.removeVolumes(ctx, fail, false)
		return errors.Join(errs...)
	}
	if !purge {
		r.say("Done. Volumes, images, binaries and", e.stateDir, "stay; --purge removes them.")
		return errors.Join(errs...)
	}
	r.say("Purging volumes, images and state")
	e.removeVolumes(ctx, fail, true)
	e.removeImages(ctx, fail, host)
	if ctrl {
		// What `stackr registry login` or a `docker login` gave the panel's
		// registry: that host's entry only, the rest of the config stays.
		try("docker", "logout", registryHost(host))
	}
	fail(r.run(ctx, "rm", "-rf", gone))
	// This installer among them: Linux keeps a running binary until it exits.
	bins := []string{filepath.Join(worker.SwapBinDir, ".stackr-swap")}
	for _, n := range worker.SwapBinaries {
		bins = append(bins, filepath.Join(worker.SwapBinDir, n), filepath.Join(worker.SwapBinDir, n+".prev"))
	}
	if ctrl {
		bins = append(bins, wrapperPath)
	}
	fail(r.run(ctx, "rm", append([]string{"-f"}, bins...)...))
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

// removeVolumes removes stackr's named volumes that are there: the proxy's
// certificates, and with all also the managed ones, the observe and
// registry data and the builder's state. Only names the box has are passed
// to rm, so a node that never had one prints no error for it.
func (e uninstallEnv) removeVolumes(ctx context.Context, fail func(error), all bool) {
	out, err := e.read(ctx, "docker", "volume", "ls", "-q")
	if err != nil {
		fail(fmt.Errorf("docker volume ls: %w", err))
		return
	}
	var managed []string
	if all {
		m, err := e.read(ctx, "docker", "volume", "ls", "-q", "--filter", managedLabel)
		if err != nil {
			fail(fmt.Errorf("docker volume ls: %w", err))
		}
		managed = strings.Fields(m)
	}
	if vs := stackrVolumes(strings.Fields(out), managed, all); len(vs) > 0 {
		fail(e.r.run(ctx, "docker", append([]string{"volume", "rm"}, vs...)...))
	}
}

// stackrVolumes picks stackr's volumes out of every name the box has.
func stackrVolumes(names, managed []string, all bool) []string {
	var vs []string
	for _, n := range names {
		switch {
		case n == installspec.ProxyVolume,
			all && (slices.Contains(managed, n) || n == installspec.ObserveVolume ||
				n == installspec.RegistryVolume || strings.HasPrefix(n, builderPrefix)):
			vs = append(vs, n)
		}
	}
	return vs
}

// registryHost is the panel's registry name, as `stackr registry login`
// derives it.
func registryHost(panelHost string) string { return "registry." + panelHost }

// removeImages removes stackr's images: the panel and observe images by
// release, whatever carries stackr's image or managed label, every image
// pulled through the registry front (localhost:<port>/...: the system
// images at any depth and the orgs' tile images) or pushed to the panel's
// registry from this box, and the outside images stackr pulls by name.
func (e uninstallEnv) removeImages(ctx context.Context, fail func(error), panelHost string) {
	var ids []string
	add := func(id string) {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, f := range []string{imageLabel, managedLabel, "reference=" + installspec.Repo, "reference=" + installspec.ObserveRepo} {
		out, err := e.read(ctx, "docker", "image", "ls", "-q", "--filter", f)
		if err != nil {
			fail(fmt.Errorf("docker image ls: %w", err))
			continue
		}
		for _, id := range strings.Fields(out) {
			add(id)
		}
	}
	out, err := e.read(ctx, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}} {{.ID}}")
	if err != nil {
		fail(fmt.Errorf("docker image ls: %w", err))
	}
	byID, byRef := stackrImages(out, panelHost)
	for _, id := range byID {
		add(id)
	}
	if len(ids) > 0 {
		fail(e.r.run(context.WithoutCancel(ctx), "docker", append([]string{"image", "rm", "-f"}, ids...)...))
	}
	// By name and without -f: an admin's container on the same image keeps
	// it, and another tag of it stays.
	if len(byRef) > 0 {
		_ = e.r.run(context.WithoutCancel(ctx), "docker", append([]string{"image", "rm"}, byRef...)...)
	}
}

// stackrImages reads `docker image ls` lines ("repo:tag id"): the ids under
// the registry front or the panel's registry host, and the pulled outside
// images present, by name.
func stackrImages(ls, panelHost string) (ids, refs []string) {
	prefixes := []string{"localhost:" + installspec.RegistryPort + "/"}
	if panelHost != "" {
		prefixes = append(prefixes, registryHost(panelHost)+"/")
	}
	for _, l := range strings.Split(ls, "\n") {
		ref, id, ok := strings.Cut(strings.TrimSpace(l), " ")
		switch {
		case !ok:
		case slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(ref, p) }):
			ids = append(ids, id)
		case ref == buildkitImage, ref == installspec.RegistryImage && panelHost != "": // only the controller runs the registry
			refs = append(refs, ref)
		}
	}
	return ids, refs
}

// iptables removes stackr's rules and chains, IPv4 and IPv6, each with one
// restore --noflush, so it applies whole or not at all and touches nothing
// it was not told to. A box with no ip6tables has no IPv6 rules to remove.
func (e uninstallEnv) iptables(ctx context.Context) error {
	var errs []error
	none := true
	for _, v := range []string{"iptables", "ip6tables"} {
		save, err := e.read(ctx, v+"-save")
		if err != nil {
			if v == "iptables" {
				errs = append(errs, fmt.Errorf("iptables-save: %w", err))
			}
			continue
		}
		if payload := iptablesPurge(save); payload != "" {
			none = false
			errs = append(errs, e.r.runIn(ctx, payload, v+"-restore", "--noflush"))
		}
	}
	if none && len(errs) == 0 {
		e.r.say("  none there")
	}
	return errors.Join(errs...)
}

// mesh deletes stackr0, the exit links and their route tables and rules, by
// the names the node saved in dir when it applied its mesh.
func (e uninstallEnv) mesh(ctx context.Context, dir string, try func(string, ...string)) {
	var spec worker.MeshSpec
	if b, err := os.ReadFile(filepath.Join(dir, worker.MeshFile)); err == nil {
		if err := json.Unmarshal(b, &spec); err != nil {
			e.r.say("  the saved mesh state is unreadable; removing stackr0 only:", err)
			spec = worker.MeshSpec{}
		}
	}
	// mesh.json is a file on the box, not trusted: only stackr's own
	// interfaces, route tables and rule priorities are touched.
	ifaces := []string{mesh.Iface}
	var tables []int
	var prefs []int
	for _, l := range spec.Links {
		ifaces, tables = append(ifaces, l.Iface), append(tables, l.Table)
	}
	for _, s := range spec.Serves {
		ifaces, tables, prefs = append(ifaces, s.Iface), append(tables, s.Table), append(prefs, s.Pref)
	}
	for _, i := range ifaces {
		if i != mesh.Iface && !exitIface.MatchString(i) {
			e.r.say("  skipping interface", fmt.Sprintf("%q", i), "from the saved mesh state: not stackr's")
			continue
		}
		try("ip", "link", "del", i)
	}
	for _, t := range tables {
		if !inRange(t, tableRanges) {
			e.r.say("  skipping route table", strconv.Itoa(t), "from the saved mesh state: outside stackr's ranges")
			continue
		}
		try("ip", "route", "flush", "table", strconv.Itoa(t))
	}
	for _, p := range prefs {
		e.delPref(try, p)
	}
	for _, l := range spec.Ladders {
		for _, st := range l.Subject.Clear() {
			if p, err := strconv.Atoi(st.Argv[len(st.Argv)-1]); err == nil {
				e.delPref(try, p)
			}
		}
	}
}

func (e uninstallEnv) delPref(try func(string, ...string), p int) {
	if !inRange(p, prefRanges) {
		e.r.say("  skipping rule priority", strconv.Itoa(p), "from the saved mesh state: outside stackr's ranges")
		return
	}
	try("ip", "rule", "del", "pref", strconv.Itoa(p))
}

// stackr's own ip numbers, from internal/service/mesh.go: link tables
// 1000+slot and serve tables 1300+slot (slot 1..254), ladder rule priorities
// from 10000 in blocks below 20000, serve priorities 20000+slot.
var (
	tableRanges = [][2]int{{1001, 1254}, {1301, 1554}}
	prefRanges  = [][2]int{{10000, 20254}}
	// exitIface is exits.Iface: wgx- and ten hex digits.
	exitIface = regexp.MustCompile("^" + exitIfacePrefix + "[0-9a-f]{10}$")
)

func inRange(n int, rs [][2]int) bool {
	return slices.ContainsFunc(rs, func(r [2]int) bool { return n >= r[0] && n <= r[1] })
}

// confirmPurge asks for the node's name, on the controller the panel's host
// name. --yes never stands in for it: a purge deletes data a rejoin would
// have found again, and on the controller the whole cluster's. The node
// remove's host helper runs `uninstall --yes`, so this is also what keeps
// it off a panel.
func (e uninstallEnv) confirmPurge(ctrl bool, panelHost, dir string) error {
	if ctrl {
		if panelHost == "" {
			return errors.New("the install names no panel host; nothing was removed")
		}
		e.r.say("--purge removes the panel, every app on this box, stackr's volumes, images and binaries and", dir, "from this box.")
		e.r.say("Type the panel's host name to go on (the name in its address):")
		line, _ := bufio.NewReader(e.in).ReadString('\n')
		if strings.TrimSpace(line) != panelHost {
			return errors.New("that is not the panel's host name; nothing was removed")
		}
		return nil
	}
	want, err := e.nodeName()
	if err != nil {
		return err
	}
	e.r.say("--purge removes stackr's volumes, images and binaries and", dir, "from this box.")
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

// checkStateDir keeps rm -rf away from anything that is not a state or data
// dir's shape: absolute, clean, at least two levels deep.
func checkStateDir(d string) error {
	if !filepath.IsAbs(d) || filepath.Clean(d) != d || strings.Count(d, "/") < 2 {
		return fmt.Errorf("%q is not a stackr state directory", d)
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
// An ip6tables-save reads the same.
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
