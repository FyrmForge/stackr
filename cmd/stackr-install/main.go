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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

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
	if err := fs.Parse(args); err != nil {
		return err
	}
	in.DNS01 = *dnsToken != ""
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
	_, _ = fmt.Fprint(out, doneText(in, newKey, r.dry))
	return nil
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

func doneText(in installspec.Input, key string, dry bool) string {
	status := "stackr is running."
	if dry {
		status = "Dry run finished, nothing was installed. A real run would end with:"
	}
	s := fmt.Sprintf(`
%s

  setup     %s   (the first account becomes the admin)
  data      %s
  admin     stackr <command>          (works when DNS or the proxy is broken)
  upgrade   Admin, Update in the panel
`, status, in.BaseURL(), in.DataDir)
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
