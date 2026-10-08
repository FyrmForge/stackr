// Command stackr-install installs stackr on a bare Docker host: data dir,
// master key, the proxy and the panel containers. Re-running it converges:
// every step looks before it writes.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// version is set at build time via ldflags: the release this binary installs.
var version = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "stackr-install:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) > 0 && args[0] == "restore" {
		return restore(ctx, args[1:], out)
	}
	fs := flag.NewFlagSet("stackr-install", flag.ContinueOnError)
	in := installspec.Input{DataDir: "/var/lib/stackr"}
	if d := os.Getenv("STACKR_DATA_DIR"); d != "" {
		in.DataDir = d
	}
	fs.StringVar(&in.Root, "domain", "", "root domain; tiles get names under it (required)")
	fs.StringVar(&in.PanelHost, "panel-host", "", "the panel's own name (default stkr.<domain>)")
	fs.BoolVar(&in.HTTPS, "https", true, "serve HTTPS with automatic certificates")
	fs.StringVar(&in.Email, "email", "", "ACME contact address (required with --https)")
	fs.StringVar(&in.Proxies, "proxy", "", "CDN or load balancer ranges whose X-Forwarded-For is trusted")
	fs.StringVar(&in.HTTPPort, "http-port", "80", "host port for HTTP")
	fs.StringVar(&in.HTTPSPort, "https-port", "443", "host port for HTTPS")
	dnsToken := fs.String("dns-token", "", "Cloudflare API token for DNS-01 (wildcard certificates)")
	release := fs.String("version", "", "release to install (default: this installer's own)")
	dry := fs.Bool("dry-run", false, "print what would change, change nothing")
	adminEmail := fs.String("admin-email", "", "the stackr admin's email; the password comes from "+passwordEnv+" or a prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	ver, err := checkVersion(*release, version)
	if err != nil {
		return err
	}
	image := installspec.Image(ver)
	r := runner{dry: *dry, out: out}

	if !r.dry {
		if err := preflight(ctx); err != nil {
			return err
		}
	}

	// A re-run converges on the saved answers; a flag given must still
	// match them (checked below). Only a fresh install makes the admin.
	// ponytail: a fresh run that fails before the admin step leaves a saved
	// install with no admin; the error says to re-run with --admin-email.
	saved, err := installspec.Load(in.DataDir)
	fresh := err != nil
	if fresh {
		in.InstallID = newInstallID()
	} else {
		fillFrom(&in, saved, given)
	}
	password := os.Getenv(passwordEnv)
	needAdmin := fresh || given["admin-email"]
	if term.IsTerminal(int(os.Stdin.Fd())) && fresh {
		a := answers{root: in.Root, panel: in.PanelHost, email: in.Email, proxies: in.Proxies, adminEmail: *adminEmail,
			https: map[bool]string{true: "Yes", false: "No"}[in.HTTPS]} // a given --https steers the follow-ups
		summary := func() string { return summaryText(a) }
		qs := installQuestions(&a, given, needAdmin, *adminEmail != "", password != "", summary)
		if err := ask(qs); err != nil {
			return err
		}
		if a.next == nextCancel {
			return errCancelled
		}
		r.dry = r.dry || a.next == nextDryRun
		apply(&in, a, given, dnsToken, adminEmail)
		if password == "" {
			password = a.password
		}
	}
	in.DNS01 = in.DNS01 || *dnsToken != ""
	if needAdmin {
		switch {
		case *adminEmail == "":
			return errors.New("the admin's email is missing: pass --admin-email, or run in a terminal to be asked")
		case password == "":
			return fmt.Errorf("the admin's password is missing: set %s, or run in a terminal to be asked", passwordEnv)
		}
		if *adminEmail, err = checkEmail(*adminEmail); err != nil {
			return fmt.Errorf("--admin-email: %w", err)
		}
		if _, err := checkPassword(password); err != nil {
			return fmt.Errorf("%s: %w", passwordEnv, err)
		}
	}
	// Our own proxy holds the ports on a re-run.
	proxyUp := r.exists(ctx, "container", installspec.ProxyName)
	busy := portBusy
	if r.dry || proxyUp {
		busy = nil
	}
	if err := check(&in, busy); err != nil {
		return err
	}
	in.Bind = r.bridgeGateway(ctx)

	// A re-run with other answers would leave the running containers
	// disagreeing with the saved ones the upgrade rebuilds from.
	if old, err := installspec.Load(in.DataDir); err == nil && !reflect.DeepEqual(old, in) {
		return fmt.Errorf("already installed with other answers (%s); change them there or reinstall from a clean data dir",
			filepath.Join(in.DataDir, installspec.File))
	}

	r.say("Installing stackr", ver)
	if err := r.mkdir(filepath.Join(in.DataDir, "keys"), 0o700); err != nil {
		return err
	}
	newKey, err := r.masterKey(in.DataDir)
	if err != nil {
		return err
	}
	if err := r.write(filepath.Join(in.DataDir, installspec.File),
		func() error { return installspec.Save(in) }); err != nil {
		return err
	}
	r.conntrackAcct()
	if err := r.pull(ctx, image); err != nil {
		return err
	}
	if !r.exists(ctx, "volume", installspec.ProxyVolume) {
		if err := r.docker(ctx, "volume", "create", installspec.ProxyVolume); err != nil {
			return err
		}
	}
	if proxyUp {
		r.say("  have", installspec.ProxyName)
	} else if err := r.docker(ctx, installspec.Proxy(image, in, *dnsToken).RunArgs()...); err != nil {
		return err
	}
	// The panel may be renamed stackr-<version> by an upgrade; its label is
	// what stays.
	if id, _ := read(ctx, "docker", "ps", "-aq", "--filter", "label="+installspec.LabelRole+"=panel"); id != "" {
		r.say("  have panel", strings.Fields(id)[0])
	} else if err := r.docker(ctx, installspec.Panel(image, in).RunArgs()...); err != nil {
		return err
	}
	if err := r.write(wrapperPath,
		func() error { return os.WriteFile(wrapperPath, []byte(wrapper), 0o755) }); err != nil {
		return err
	}
	if needAdmin {
		if err := r.createAdmin(ctx, in, image, *adminEmail, password); err != nil {
			return fmt.Errorf("admin: %w; re-run with --admin-email to try again", err)
		}
	}
	admin := ""
	if needAdmin {
		admin = *adminEmail
	}
	_, _ = fmt.Fprint(out, doneText(in, newKey, admin, r.dry))
	return nil
}

// passwordEnv carries the admin password on an unattended run: never a flag,
// which shell history and ps would keep.
const passwordEnv = "STACKR_ADMIN_PASSWORD"

// fillFrom takes each answer not given as a flag from the saved install.
func fillFrom(in *installspec.Input, saved installspec.Input, given map[string]bool) {
	for flag, pick := range map[string]func(){
		"domain":     func() { in.Root = saved.Root },
		"panel-host": func() { in.PanelHost = saved.PanelHost },
		"https":      func() { in.HTTPS = saved.HTTPS },
		"email":      func() { in.Email = saved.Email },
		"proxy":      func() { in.Proxies = saved.Proxies },
		"http-port":  func() { in.HTTPPort = saved.HTTPPort },
		"https-port": func() { in.HTTPSPort = saved.HTTPSPort },
		"dns-token":  func() { in.DNS01 = saved.DNS01 },
	} {
		if !given[flag] {
			pick()
		}
	}
	in.InstallID = saved.InstallID
}

// apply copies the asked answers into the install; given flags stay.
func apply(in *installspec.Input, a answers, given map[string]bool, dnsToken, adminEmail *string) {
	in.Root, in.PanelHost, in.Email, in.Proxies = a.root, a.panel, a.email, a.proxies
	if !given["https"] {
		in.HTTPS = a.https != "No"
	}
	if !given["dns-token"] && a.wildcard == "Yes" {
		*dnsToken = a.dnsToken
	}
	if a.adminEmail != "" {
		*adminEmail = a.adminEmail
	}
}

// summaryText is what the install will be, shown before the last pick.
func summaryText(a answers) string {
	scheme := "https"
	if a.https == "No" {
		scheme = "http"
	}
	s := fmt.Sprintf("\n  Panel   %s://%s\n  Tiles   *.%s\n", scheme, a.panel, strings.TrimPrefix(a.root, "*."))
	if a.adminEmail != "" {
		s += fmt.Sprintf("  Admin   %s\n", a.adminEmail)
	}
	return s
}

// createAdmin waits for the panel, then makes the admin inside it; the
// password goes in on stdin so no flag, env or inspect ever holds it.
func (r runner) createAdmin(ctx context.Context, in installspec.Input, image, email, password string) error {
	if r.dry {
		r.say("  would create the admin", email)
		return nil
	}
	if err := waitHealthy(ctx, "http://"+net.JoinHostPort(in.Bind, installspec.PanelPort)+"/api/health"); err != nil {
		return err
	}
	panel, err := read(ctx, "docker", "ps", "-q", "--filter", "label="+installspec.LabelRole+"=panel")
	if err != nil || panel == "" {
		return errors.New("the panel container is not running")
	}
	panel = strings.Fields(panel)[0]
	// An older panel has no create-admin: its stackrd would boot a second
	// server instead. Only this installer's own image is asked.
	if got, _ := read(ctx, "docker", "inspect", "-f", "{{.Config.Image}}", panel); got != image {
		return fmt.Errorf("the panel runs %s, not %s; upgrade it from the panel first", got, image)
	}
	check := exec.CommandContext(ctx, "docker", "exec", panel, "/stackrd", "create-admin", "--check")
	if err := check.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			r.say("  have an admin")
			return nil
		}
		return fmt.Errorf("check for an admin: %w", err)
	}
	r.say("  creating the admin", email)
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", panel, "/stackrd", "create-admin", "--email", email)
	cmd.Stdin = strings.NewReader(password + "\n")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return nil
}

// waitHealthy polls the panel's health route; a first boot migrates first.
func waitHealthy(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the panel did not come up at %s within 90s; see docker logs %s", url, installspec.PanelName)
		case <-time.After(time.Second):
		}
	}
}

// wrapper runs the CLI inside the panel: with a domain the panel publishes
// nothing, so when the proxy or DNS is broken this is the way in.
// step 4: the CLI speaks HTTP only, so /stackr must dial $HOST:$PORT from
// the panel's env (the bridge gateway, not localhost) and needs a way to
// authenticate from inside the container.
const wrapperPath = "/usr/local/bin/stackr"

const wrapper = `#!/bin/sh
exec docker exec -i "$(docker ps -q --filter label=stackr.role=panel | head -n1)" /stackr "$@"
`

// preflight refuses a box that cannot be installed on.
func preflight(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("run as root (docker, /var/lib and port 80 all need it)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker is not installed; https://docs.docker.com/engine/install/")
	}
	if _, err := read(ctx, "docker", "info"); err != nil {
		return errors.New("cannot talk to the docker daemon; is it running?")
	}
	return nil
}

// portBusy dials first: in a non-root run a low port cannot be bound
// whether it is taken or not.
func portBusy(port string) bool {
	if c, err := net.Dial("tcp", "127.0.0.1:"+port); err == nil {
		_ = c.Close()
		return true
	}
	l, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return true
	}
	_ = l.Close()
	return false
}

// runner is the only thing that changes the box. dry prints each change
// instead; reads always run for real.
type runner struct {
	dry bool
	out io.Writer
}

func (r runner) say(a ...any) { _, _ = fmt.Fprintln(r.out, a...) }

func (r runner) docker(ctx context.Context, args ...string) error {
	if r.dry {
		r.say("  would run: docker", quote(args))
		return nil
	}
	r.say("  docker", strings.Join(args[:min(2, len(args))], " "))
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = r.out, r.out
	return cmd.Run()
}

func (r runner) write(path string, do func() error) error {
	if r.dry {
		r.say("  would write", path)
		return nil
	}
	return do()
}

func (r runner) mkdir(path string, mode os.FileMode) error {
	if r.dry {
		r.say("  would create", path)
		return nil
	}
	return os.MkdirAll(path, mode)
}

// masterKey writes 32 random bytes as hex unless a key is already there.
// Reports whether it made one: the recovery passphrase is printed once.
func (r runner) masterKey(dataDir string) (string, error) {
	path := filepath.Join(dataDir, installspec.KeyFile)
	if _, err := os.Stat(path); err == nil {
		return "", nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := hex.EncodeToString(b)
	if r.dry {
		key = "<made on a real run>"
	}
	return key, r.write(path, func() error {
		return os.WriteFile(path, []byte(key+"\n"), 0o600)
	})
}

// Conntrack byte counters feed the tile traffic lanes: on now, and after a
// reboot. Only connections opened from here on are counted. A box that
// refuses (no conntrack module, a read-only /proc) still installs; the panel
// warns at boot.
const (
	acctKnob = "/proc/sys/net/netfilter/nf_conntrack_acct"
	acctConf = "/etc/sysctl.d/99-conntrack-acct.conf"
)

func (r runner) conntrackAcct() {
	for _, f := range [][2]string{
		{acctKnob, "1\n"},
		{acctConf, "net.netfilter.nf_conntrack_acct=1\n"},
	} {
		path, body := f[0], f[1]
		if err := r.write(path, func() error { return os.WriteFile(path, []byte(body), 0o644) }); err != nil {
			r.say("  warning: tile traffic stays empty:", err)
		}
	}
}

// pull skips an image already here: a release tag never changes, and it lets
// a box without registry access install an image loaded by hand.
func (r runner) pull(ctx context.Context, image string) error {
	if r.exists(ctx, "image", image) {
		r.say("  have", image)
		return nil
	}
	return r.docker(ctx, "pull", image)
}

func (r runner) exists(ctx context.Context, kind, name string) bool {
	_, err := read(ctx, "docker", kind, "inspect", name)
	return err == nil
}

// bridgeGateway is the default bridge's gateway, where the panel listens and
// what `host-gateway` resolves to inside the proxy.
func (r runner) bridgeGateway(ctx context.Context) string {
	gw, err := read(ctx, "docker", "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil || gw == "" {
		return "172.17.0.1" // docker's default; a dry run off-box lands here
	}
	return gw
}

func read(ctx context.Context, name string, args ...string) (string, error) {
	b, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(b)), err
}

// quote breaks the line before every flag so a container create is readable.
func quote(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			if strings.HasPrefix(a, "-") {
				b.WriteString(" \\\n      ")
			} else {
				b.WriteByte(' ')
			}
		}
		if strings.ContainsAny(a, " \t'\"$") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		b.WriteString(a)
	}
	return b.String()
}

// doneText closes the run; admin is the account made this run, if any.
func doneText(in installspec.Input, key, admin string, dry bool) string {
	status := "stackr is running."
	if dry {
		status = "Dry run finished, nothing was installed. A real run would end with:"
	}
	login := ""
	if admin != "" {
		login = fmt.Sprintf("  log in    %s with the password you chose\n", admin)
	}
	s := fmt.Sprintf(`
%s

  panel     %s
%s  data      %s
  cli       stackr <command>          (works when DNS or the proxy is broken)
  upgrade   Admin, Update in the panel
  people    invite them from the org's Members tab
`, status, in.BaseURL(), login, in.DataDir)
	if key != "" {
		s += fmt.Sprintf(`
Recovery passphrase, shown once. Save it somewhere off this box: it opens
the panel's own backups, and restoring them anywhere else needs it.

  %s
`, key)
	}
	if !in.HTTPS {
		return s
	}
	root := strings.TrimPrefix(in.Root, "*.")
	w := max(len(in.PanelHost), len(root)+2)
	return s + fmt.Sprintf(`
DNS: two records, both pointing at this server:

  %-*s  A    <this server's public IP>
  %-*s  A    <this server's public IP>

The second is not optional. Stacks get hostnames under %s, and each
one needs to resolve before a certificate can be issued for it.

Until DNS is live there is no browser route in: the panel publishes no port,
so everything arrives through Caddy. Use the admin CLI on this box instead:

  stackr --help
`, w, in.PanelHost, w, "*."+root, root)
}

// newInstallID names this box's panel archives in a shared bucket.
func newInstallID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail
	}
	return hex.EncodeToString(b)
}
