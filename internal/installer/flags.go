package installer

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"charm.land/huh/v2"
)

// releaseVersion matches what semantic release tags: 0.1.5, 0.1.5-rc.1.
var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// version is the release this installer came from, set at build time. It
// installs that release unless --version says otherwise, so no GitHub API
// call is needed to find the latest.
var version = "dev"

// options is everything on the command line besides the answers.
type options struct {
	version string
	dry     bool
	yes     bool
}

// parseFlags maps the command line onto the answers. Every question has a
// flag; with the form they are the starting values.
func parseFlags(args []string, errOut io.Writer) (*Answers, options, error) {
	a := &Answers{HTTPS: true, HTTPPort: "80", HTTPSPort: "443"}
	var o options
	fs := flag.NewFlagSet("stackr-install", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&a.Root, "domain", "", "root domain; apps get names under it")
	fs.StringVar(&a.Host, "panel-host", "", "panel hostname (default stkr.<domain>)")
	fs.BoolVar(&a.Cloudflare, "cloudflare", false, "the domain is behind Cloudflare")
	fs.StringVar(&a.Proxies, "proxy", "", "your proxy's IP addresses or ranges, comma separated")
	fs.BoolVar(&a.HTTPS, "https", true, "Let's Encrypt certificates (--https=false for plain HTTP)")
	fs.StringVar(&a.Email, "email", "", "email for Let's Encrypt")
	fs.StringVar(&a.HTTPPort, "http-port", a.HTTPPort, "HTTP port")
	fs.StringVar(&a.HTTPSPort, "https-port", a.HTTPSPort, "HTTPS port")
	fs.StringVar(&o.version, "version", version, "stackr release to install")
	fs.BoolVar(&o.dry, "dry-run", false, "ask everything, print what would run, change nothing")
	fs.BoolVar(&o.yes, "yes", false, "no form: install from the flags, fail on a missing or bad one")
	if err := fs.Parse(args); err != nil {
		return nil, o, err
	}
	if fs.NArg() > 0 {
		return nil, o, fmt.Errorf("unknown argument: %s", fs.Arg(0))
	}
	// "latest" is the release this binary came from: install.sh downloaded it
	// from the latest release to begin with.
	if o.version == "latest" {
		o.version = version
	}
	o.version = strings.TrimPrefix(o.version, "v")
	if o.version == "dev" || o.version == "" {
		return nil, o, errors.New("this installer was built without a release version; pass --version X.Y.Z")
	}
	// It becomes an image tag; a typo here fails much later, at the pull.
	if !releaseVersion.MatchString(o.version) {
		return nil, o, fmt.Errorf("%q is not a release version; pass one like 0.1.5", o.version)
	}
	if o.yes && a.Root == "" {
		return nil, o, errors.New("--yes needs --domain")
	}
	a.DataDir = os.Getenv("STACKR_DATA_DIR")
	if a.DataDir == "" {
		// Not asked: nearly everyone wants the default. STACKR_DATA_DIR moves
		// it, for a bigger disk or a path backups already cover.
		a.DataDir = "/var/lib/stackr"
	}
	return a, o, nil
}

// Main runs stackr-install and returns the exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, args, os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, out io.Writer) error {
	a, o, err := parseFlags(args, os.Stderr)
	if err != nil {
		return err
	}
	img := Images{
		Panel: envOr("STACKR_IMAGE", "ghcr.io/fyrmforge/stackr:"+o.version),
		Relay: envOr("STACKR_RELAY_IMAGE", "ghcr.io/fyrmforge/stackr-proxyrelay:"+o.version),
	}

	if o.dry {
		_, _ = fmt.Fprintln(out, "dry run: nothing on this machine is changed")
	} else if err := Preflight(ctx); err != nil {
		return err
	}
	busy := portBusy
	if o.dry {
		busy = nil
	}

	if o.yes {
		if err := a.Check(busy); err != nil {
			return err
		}
		for _, w := range dnsWarnings(ctx, a, lookupHost) {
			_, _ = fmt.Fprintln(out, w)
		}
	} else {
		if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return errors.New("no terminal to show the install form on; run it from an interactive shell (ssh -t), or pass --yes with flags")
		}
		if err := formLoop(ctx, a, o.dry, busy); err != nil {
			return err
		}
	}

	if o.dry {
		for _, p := range []string{a.HTTPPort, a.HTTPSPort} {
			if p != "" && portBusy(p) {
				_, _ = fmt.Fprintf(out, "warning: something on this machine listens on %s; a real install here would refuse it\n", p)
			}
		}
	}
	if err := Install(ctx, a, img, o.dry, out); err != nil {
		return err
	}
	_, _ = fmt.Fprint(out, doneText(a, o.dry))
	return nil
}

// formLoop runs the form and the summary until the operator installs or
// cancels. Go back keeps every answer.
func formLoop(ctx context.Context, a *Answers, dry bool, busy func(string) bool) error {
	for {
		if err := ask(a, dry); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return errors.New("cancelled, nothing was installed")
			}
			return err
		}
		checked := *a
		if err := checked.Check(busy); err != nil {
			// The form checks every field as it goes; this catches one that
			// changed under it, like a port taken while the form was open.
			_, _ = fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}
		choice, err := confirm(&checked, dnsWarnings(ctx, &checked, lookupHost), dry)
		if err != nil && !errors.Is(err, huh.ErrUserAborted) {
			return err
		}
		switch {
		case err != nil || choice == ChoiceCancel:
			return errors.New("cancelled, nothing was installed")
		case choice == ChoiceInstall:
			*a = checked
			return nil
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func doneText(a *Answers, dry bool) string {
	status := "stackr is running."
	if dry {
		status = "Dry run finished, nothing was installed. A real run would end with:"
	}
	s := fmt.Sprintf(`
%s

  panel     %s
  data      %s
  admin     stackr <command>          (works even when the panel is unreachable)
  upgrade   Admin, Update in the panel
`, status, a.BaseURL(), a.DataDir)
	if !a.TLS() {
		return s
	}
	// Both names in one column, whatever their length.
	w := max(len(a.PanelHost()), len(a.Root)+2)
	return s + fmt.Sprintf(`
DNS: two records, both pointing at this server:

  %-*s  A    <this server's public IP>
  %-*s  A    <this server's public IP>

The second is not optional. Stacks get hostnames under %s, and each
one needs to resolve before a certificate can be issued for it.

Until DNS is live there is no browser route in: the panel publishes no port,
so everything arrives through Traefik. Use the admin CLI on this box instead:

  stackr --help
`, w, a.PanelHost(), w, "*."+a.Root, a.Root)
}
