# installer

Source: `installer/` (answers.go, flags.go, form.go, steps.go, answers_test.go), `installspec/` (installspec.go, installspec_test.go)
Commit: c2423f0
Taken: the host install steps in order, the root/panel-host grammar and the rest of the answer checks, the one spec both install and upgrade build the panel from, the first-run output, the idempotence guards.
Cut: everything Swarm (init, overlay address pool, advertise address, overlay network, `service create`), the insecure-registry `daemon.json` merge and dockerd restart, the proxyrelay image pull and retag, the huh TUI form and its recap/summary rendering, the DNS pre-check lookups.
Cuts belong to: Swarm bits are gone for good (REWRITE.md); the registry trust belongs with the managed-registry row; the form belongs in leaf/cli; the DNS pre-check is optional polish, not install.

## The spec both install and upgrade build from

`installspec` exists because the service spec used to live only in `installer/steps.go`,
with a hand copy in a deploy script — so a new environment variable reached fresh
installs and never an upgraded one. Keep that property: one package, no store, no
Docker call, standard library only (the installer is a separate binary that runs
before there is a database).

```go
// installspec/installspec.go
const (
	ServiceName = "stackr"
	Hostname    = "stkr-panel"
	// StopGrace is generous: a shutdown mid-deploy has containers to settle.
	StopGrace = "120s"
)
// extract: dropped Network/Constraint/Replicas/RelayPort, Swarm and relay only.

// Input is everything about this install the spec varies on. A plain struct
// rather than the installer's Answers, so the panel can build one for upgrade.
type Input struct {
	BaseURL   string
	Email     string
	TLS       bool
	Root      string
	TrustCF   bool
	Proxies   string
	DataDir   string
	HTTPPort  string
	HTTPSPort string
}

// Env is the panel's environment, in a fixed order so two installs of the same
// answers produce the same container.
func Env(in Input) [][2]string {
	tls, trustCF := "off", ""
	if in.TLS {
		tls = "on"
	}
	if in.TrustCF {
		trustCF = "1"
	}
	env := [][2]string{
		{"BASE_URL", in.BaseURL},
		{"ACME_EMAIL", in.Email},
		{"STACKR_TLS", tls},
		{"ROOT_DOMAIN", in.Root},
		{"TRUST_CLOUDFLARE", trustCF},
		{"TRUSTED_PROXY_CIDRS", in.Proxies},
		{"DATA_DIR", in.DataDir},
		{"DATABASE_PATH", in.DataDir + "/stackr.db"},
		{"TRAEFIK_HTTP_PORT", in.HTTPPort},
		{"HOST_PROC", "/host/proc"},
	}
	// With HTTPS off there is no HTTPS port; nothing publishes one.
	if in.HTTPSPort != "" {
		env = append(env, [2]string{"TRAEFIK_HTTPS_PORT", in.HTTPSPort})
	}
	return env
}
// extract: the two port vars keep their TRAEFIK_ names here; renaming them is a
// builder decision that touches whatever reads them, not a fact from this row.

// CreateArgs is the panel's create, image included.
func CreateArgs(image string, in Input) []string {
	args := []string{"service", "create",
		"--name", ServiceName,
		"--hostname", Hostname,
		// extract: dropped --network/--constraint/--replicas, Swarm only.
		// extract: new work, see REWRITE.md "Infrastructure" — host networking
		// and NET_ADMIN replace those flags, and they also replace the
		// --publish mode=host branch (Input.HostPublish, the escape hatch for a
		// box with no domain: swarm published ports have no host IP, so a
		// loopback-only 127.0.0.1:8080 could not exist on a service).
	}
	args = append(args,
		"--mount", "type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock",
		"--mount", "type=bind,src=/proc,dst=/host/proc,ro",
		"--mount", "type=bind,src="+in.DataDir+",dst="+in.DataDir,
	)
	for _, kv := range append(Env(in), [2]string{"STACKR_IMAGE", image}) {
		args = append(args, "--env", kv[0]+"="+kv[1])
	}
	return append(args, "--stop-grace-period", StopGrace, image)
}
```

The three mounts, the env loop, `STACKR_IMAGE` and `StopGrace` are the payload
and survive whatever the outer command becomes.

STACKR_IMAGE is set to the image the panel was created from: the panel reads it
back to know its own version and to build the upgrade from.

## The root/domain grammar

One grammar, in `installspec`, used by the installer *and* the panel's own domain
form. Before it existed the installer refused `localhost` and a bare IP while the
panel's laxer rule accepted both, so a box could hold a domain resource its own
installer would have rejected.

```go
// CleanHost takes what people paste: a browser URL with scheme, path, port, a
// trailing dot, capitals.
func CleanHost(s string) string {
	h := strings.TrimSpace(s)
	h = strings.TrimPrefix(strings.TrimPrefix(h, "http://"), "https://")
	h, _, _ = strings.Cut(h, "/")
	h, _, _ = strings.Cut(h, ":")
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// hostChars is every character a DNS label may contain.
const hostChars = "abcdefghijklmnopqrstuvwxyz0123456789-"

// CheckRoot accepts a domain name Let's Encrypt can issue for, cleaned. A
// leading "*." is allowed: a wildcard is a name like any other with one label
// standing in for the rest.
func CheckRoot(s string) (string, error) {
	h := CleanHost(s)
	wildcard := strings.HasPrefix(h, "*.")
	bare := strings.TrimPrefix(h, "*.")
	if bare == "" {
		return "", errors.New("a domain is required")
	}
	if _, err := netip.ParseAddr(bare); err == nil || bare == "localhost" {
		return "", fmt.Errorf("%s is an address, not a domain; stackr reaches tiles and the panel by name", bare)
	}
	if len(h) > 253 {
		return "", fmt.Errorf("%s is longer than 253 characters", h)
	}
	labels := strings.Split(bare, ".")
	for _, l := range labels {
		switch {
		case l == "":
			return "", fmt.Errorf("%s has an empty label", h)
		case len(l) > 63:
			return "", fmt.Errorf("%s is longer than 63 characters", l)
		case strings.Trim(l, hostChars) != "":
			return "", fmt.Errorf("%s has characters a domain cannot contain", h)
		case l[0] == '-' || l[len(l)-1] == '-':
			return "", fmt.Errorf("%s starts or ends with a dash", l)
		}
	}
	if len(labels) < 2 {
		return "", fmt.Errorf("%s is a single label; use a full name like example.com", h)
	}
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", fmt.Errorf("%s is not a valid domain", h)
	}
	if wildcard {
		return "*." + bare, nil
	}
	return bare, nil
}
```

Refused: `""`, `localhost`, `127.0.0.1`, `::1`, `example`, `ex ample.com`,
`-bad.com`, `a..b.com`, `example.123`, a 64-character label.
Accepted and cleaned: `https://Example.COM/path` → `example.com`,
`example.com.` → `example.com`, `example.com:8443` → `example.com`,
`*.apps.example.com` kept as is.

## The answers and the checks

```go
// installer/answers.go
// Answers is everything the install needs from the operator. Form and flags
// both fill it; Check is the one place that decides it is usable. A domain is
// not optional: tiles are reached by name, and so is the panel. An install on a
// bare IP would have no names, no certificates and no route to the panel but a
// published port — a different product, not a flag.
type Answers struct {
	// Tiles get names under Root; the panel its own Host under it. Blank Host
	// means stkr.<Root>.
	Root       string
	Host       string
	Cloudflare bool
	Proxies    string
	HTTPS      bool
	Email      string
	HTTPPort   string
	HTTPSPort  string
	DataDir    string
}

func (a *Answers) PanelHost() string {
	if a.Host == "" {
		return "stkr." + a.Root
	}
	return a.Host
}

func (a *Answers) TLS() bool { return a.HTTPS }

// BaseURL is how the operator reaches the panel after the install.
func (a *Answers) BaseURL() string {
	switch {
	case a.TLS():
		return "https://" + a.PanelHost()
	case a.HTTPPort != "80":
		return "http://" + a.PanelHost() + ":" + a.HTTPPort
	}
	return "http://" + a.PanelHost()
}

// Check normalises every answer in place and returns the first problem.
// busy reports a port something already listens on; nil skips that check.
func (a *Answers) Check(busy func(string) bool) error {
	var err error
	if a.DataDir, err = CheckDataDir(a.DataDir); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	if a.Root, err = installspec.CheckRoot(a.Root); err != nil {
		return fmt.Errorf("domain: %w", err)
	}
	if a.Host != "" {
		if a.Host, err = CheckHost(a.Host, a.Root); err != nil {
			return fmt.Errorf("panel host: %w", err)
		}
	}
	if a.Proxies, err = CheckProxies(a.Proxies); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	if a.TLS() {
		if a.Email, err = CheckEmail(a.Email); err != nil {
			return fmt.Errorf("email: %w", err)
		}
	} else {
		a.Email, a.HTTPSPort = "", ""
	}
	if err = CheckPort(a.HTTPPort, busy); err != nil {
		return fmt.Errorf("http port: %w", err)
	}
	if a.TLS() {
		if err = CheckHTTPSPort(a.HTTPSPort, a.HTTPPort, busy); err != nil {
			return fmt.Errorf("https port: %w", err)
		}
	}
	return nil
}

// CheckHost accepts a name at or under root.
func CheckHost(s, root string) (string, error) {
	h, err := installspec.CheckRoot(s)
	if err != nil {
		return "", err
	}
	if h != root && !strings.HasSuffix(h, "."+root) {
		return "", fmt.Errorf("%s is not under %s", h, root)
	}
	return h, nil
}
```

The four small ones, kept for their rules rather than their bodies:

- `CheckProxies` — addresses or ranges split on commas or spaces, each through
  the shared trusted-CIDR parser, de-duplicated, comma-joined. Blank is valid.
  `192.168.1.100` becomes `192.168.1.100/32`; `0.0.0.0/0` and `::/0` are refused.
- `CheckEmail` — one `@`, no spaces, something before the `@`, and the domain
  through `CheckRoot`, so `ops@localhost` and `ops@1.2.3.4` are out. Lower-cased.
- `CheckPort` — 1-65535, first digit not `0` (so `080` is refused), and `busy`
  refuses one already listening.  `CheckHTTPSPort` adds "not the HTTP port".
- `CheckDataDir` — absolute, cleaned, not `/`, letters/digits/`._-/` only. The
  character rule is not cosmetic: the path goes into a `--mount` spec where a
  comma would split it.

`portBusy` dials `127.0.0.1:<port>` first and only then tries to listen, because
in a non-root dry run a low port cannot be bound whether it is taken or not.

## Preflight and the idempotence guards

```go
// installer/steps.go
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

Upgrade from the panel: Admin, Update.`)
	}
	return nil
}
// extract: the already-installed guard in its Swarm form; the container
// equivalent is new work.
// extract: dropped the by-hand recovery commands from that message, they named
// `docker service update` and the relay retag.
```

Already-present pull skip, the other half of a safe re-run:

```go
// A release tag never changes, so an image already here is the one that would
// be pulled. Also what lets a box without registry access install images loaded
// by hand.
if _, err := read(ctx, "docker", "image", "inspect", ref); err == nil {
	fmt.Fprintln(out, "  have", ref)
} else if err := r.run(ctx, true, "docker", "pull", ref); err != nil {
	return err
}
```

## The dry run

`runner{dry, out}` is the only thing that touches the box: `run` executes a
change or, with `dry`, prints `would run: <cmd>` and returns nil. Reads always
run for real. `quoteCmd` breaks the line before every `--flag`, so a container
create is readable. Worth keeping — `--dry-run` is how the installer is tested
off-box, and the check loop takes `busy == nil` in that mode for the same reason.

## First-run output

```go
// installer/flags.go
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
```

The setup URL is `a.BaseURL()` — `https://stkr.<root>` with TLS on, otherwise
`http://<panel host>[:<http port>]`. There is no token in it; first-run
account creation is the panel's own job. "Traefik" in that text is the old
proxy's name and is the one word in it the rewrite has to change.

## Install steps as today

1. **Preflight** (skipped in a dry run): root, `docker` on `PATH`, the daemon
   answers `docker info`, and no `stackr` is already installed. The
   installer never *installs* Docker — the shell bootstrap that downloads this
   binary does that; in Go it is a check with a link.
2. **Collect and check the answers.** Flags fill `Answers` (`--domain`,
   `--panel-host`, `--cloudflare`, `--proxy`, `--https`, `--email`,
   `--http-port`, `--https-port`, plus `--version`, `--dry-run`, `--yes`);
   `STACKR_DATA_DIR` moves the data dir off `/var/lib/stackr` and is never
   asked. `--yes` checks them and installs; otherwise the form asks, and both
   end at `Answers.Check`.
3. *(cut)* Swarm init with a hand-picked overlay address pool.
4. *(cut)* Merge the box's own registry into `/etc/docker/daemon.json` and
   restart dockerd, then wait up to 30s for it to answer again.
5. **Create the data dir**: `install -d -m 755 <DataDir>`. Root-owned, root-run;
   there is no stackr user and no chown. The panel container runs as root with
   the Docker socket bind-mounted, which is what gives it the box.
6. *(cut)* Create the `stkr` overlay network, refusing to continue if a
   same-named bridge network already exists.
7. **Pull the images**, skipping any whose ref `docker image inspect` already
   finds. Today that is the panel and the proxyrelay; the relay is then retagged
   to a local tag the runtime looks it up by.
   `// extract: new work, see REWRITE.md "Infrastructure"` — the proxy image
   (`stackrd proxy`) is the second image pulled here now, and its named Caddy
   volume is created alongside the data dir in step 5.
8. **Start the panel** from `installspec.CreateArgs` — today a `docker service
   create`, and the same spec the upgrade rebuilds from.
   `// extract: new work, see REWRITE.md "Infrastructure"` — host networking and
   `NET_ADMIN` go on this container, and the proxy container starts right after
   it with the Caddy volume mounted.
9. **Write the CLI wrapper** to `/usr/local/bin/stackr`: a two-line `sh` script
   that `docker exec`s `/app/stackr` inside the panel container. With a domain
   the panel publishes nothing, so when the proxy or DNS is broken this is the
   way in. Today it finds the container by swarm service label; it becomes
   `docker exec -i stackr /app/stackr "$@"`.
10. **Print `doneText`**: the setup URL, the data dir, the admin and upgrade
    lines, and — with TLS on — the two A records (`<panel host>` and `*.<root>`)
    that must resolve before certificates can be issued.

## Notes for the builder

- No restore-on-failed-swap script exists in this row. The installer creates the
  panel once and has no swap; the in-place replace lives with the upgrade code.
  If the rewrite wants a rollback, it is new work, not a port.
- The install is idempotent only in the "refuse politely" sense: Preflight stops
  a second run dead rather than converging the box. The three steps that *are*
  re-runnable (`install -d`, the pull skip, and the cut inspect-then-create
  network) are each an inspect before the write. Keep that shape for the proxy
  container and its volume.
- `installspec` must keep importing nothing but the standard library. The
  installer is a separate binary that runs before there is a database, and
  anything this package pulls in, that binary carries.
- The env list is the contract between install and upgrade, and it is the thing
  that rotted last time. Whatever check replaces `TestCreateArgsCarriesTheImage`
  should fail when a var is added on one side only. The two `TRAEFIK_*` port
  names are kept verbatim above for that reason: renaming them for Caddy is a
  builder decision that touches every reader of those vars, not something this
  row settles.
- Version handling: the installer installs the release it was built from, so no
  API call is needed to find "latest"; `--version` overrides, `v` is stripped,
  `latest` means "this binary's release", and the string is matched against
  `^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$` before it ever becomes an image
  tag — a typo otherwise fails much later, at the pull, and a `;`-bearing string
  would reach a command line.
- `--yes` needs `--domain`, and a non-`--yes` run needs a terminal: the form
  refuses a piped stdin with "run it from an interactive shell (ssh -t), or pass
  --yes with flags".
- The TUI form is cut, but two of its behaviours are worth re-creating cheaply
  wherever the answers get asked: the summary screen offers install / go back /
  cancel and keeps every answer on a go-back, and `Check` runs again after the
  summary because a port can be taken while the form is open.
- Also cut: the DNS pre-check that looks up the panel host and a random name
  under the root and warns (never blocks) when either does not resolve. It only
  runs with TLS on. Nice, not load-bearing.

Size: source 1426 lines, extract 451 lines
