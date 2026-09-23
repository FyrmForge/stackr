// Package installspec is the panel's own shape, defined once for the two
// things that create it: the installer, which runs on a bare box, and
// AdminService.Upgrade, which replaces the running service in place.
//
// It existed in neither place before — the service spec lived only in
// internal/installer/steps.go, with a hand copy in scripts/dev/deploy-test.sh,
// so a new environment variable reached fresh installs and never an upgraded
// one. The root-domain grammar was the same story from the other side: the
// installer refused localhost and a bare address, the panel's own domain form
// accepted both.
//
// It imports nothing but the standard library on purpose. The installer is a
// separate binary that runs before there is a database, so anything this
// package pulls in, that binary carries.
package installspec

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// The panel is a swarm service like everything else stackr runs, pinned to
// the manager: it holds the docker socket and the data dir, both this
// machine's.
const (
	ServiceName = "stackr"
	Network     = "stkr"
	Hostname    = "stkr-panel"
	Constraint  = "node.role == manager"
	Replicas    = "1"
	// StopGrace is generous: a shutdown mid-deploy has containers to settle.
	StopGrace = "120s"
	// RelayPort is the port cmd/proxyrelay listens on and the runtime
	// publishes. Shared for the same reason the service spec is.
	RelayPort = 15000
)

// Input is everything about this install the spec varies on. It is a plain
// struct rather than the installer's Answers so the panel can build one.
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
	// HostPublish, when set, publishes the panel on this host port. install.sh
	// never does — reaching the panel is traefik's job — but a box with no
	// domain has no other way in.
	HostPublish string
}

// Env is the panel's environment, in a fixed order so two installs of the
// same answers produce the same service.
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
	// With HTTPS off there is no HTTPS port; traefik publishes none.
	if in.HTTPSPort != "" {
		env = append(env, [2]string{"TRAEFIK_HTTPS_PORT", in.HTTPSPort})
	}
	return env
}

// CreateArgs is the panel's `docker service create`, image included.
//
// It publishes no port unless asked: reaching the panel is traefik's job, and
// when traefik or DNS is broken the way in is ssh plus the `stackr` CLI.
// Swarm published ports have no host IP anyway, so a loopback-only
// 127.0.0.1:8080 cannot exist on a service.
func CreateArgs(image string, in Input) []string {
	args := []string{"service", "create",
		"--name", ServiceName,
		"--network", Network,
		"--hostname", Hostname,
		"--constraint", Constraint,
		"--replicas", Replicas,
	}
	if in.HostPublish != "" {
		// mode=host, not the routing mesh: swarm published ports have no host
		// IP, and host mode at least keeps the port on this node's interface.
		args = append(args, "--publish", "mode=host,published="+in.HostPublish+",target=8080")
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

// CleanHost takes what people paste: a browser URL with scheme, path, port, a
// trailing dot, capitals.
func CleanHost(s string) string {
	h := strings.TrimSpace(s)
	h = strings.TrimPrefix(strings.TrimPrefix(h, "http://"), "https://")
	h, _, _ = strings.Cut(h, "/")
	h, _, _ = strings.Cut(h, ":")
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// hostChars is every character a DNS label may contain. Anything else in a
// name is a paste that went wrong.
const hostChars = "abcdefghijklmnopqrstuvwxyz0123456789-"

// CheckRoot accepts a domain name Let's Encrypt can issue for, and returns it
// cleaned. A leading "*." is allowed: a wildcard resource is a name like any
// other with one label standing in for the rest.
//
// The installer has always had this. The panel had a laxer rule of its own,
// which is how localhost and a bare IP could be claimed as domain resources
// on a box whose installer had refused exactly those.
func CheckRoot(s string) (string, error) {
	h := CleanHost(s)
	wildcard := strings.HasPrefix(h, "*.")
	bare := strings.TrimPrefix(h, "*.")
	if bare == "" {
		return "", errors.New("a domain is required")
	}
	if _, err := netip.ParseAddr(bare); err == nil || bare == "localhost" {
		return "", fmt.Errorf("%s is an address, not a domain; stackr reaches apps and the panel by name", bare)
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
