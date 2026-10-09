// Package installspec is the one description of the panel and proxy
// containers. The installer creates them from it and the panel's
// self-upgrade rebuilds its own container from it, so an env var added here
// reaches fresh installs and upgraded ones alike. Standard library only: the
// installer is a separate binary that runs before there is a database.
package installspec

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	PanelName   = "stackr"
	ProxyName   = "stackr-proxy" // service.ProxyContainer
	ProxyVolume = "stackr-caddy"
	Repo        = "ghcr.io/fyrmforge/stackr"
	LabelRole   = "stackr.role" // panel | proxy; the upgrade finds the panel by it
	PanelPort   = "8080"
	// LabelSpec stamps a container with the Hash of the spec it was made
	// from, so a re-run or upgrade can tell a stale proxy from a current one.
	LabelSpec = "stackr.spec"
	// DNSTokenEnv is the proxy's DNS-01 token variable.
	DNSTokenEnv = "DNS_API_TOKEN"
	// ProxyAdminDir is where the proxy container sees the directory that
	// holds Caddy's admin socket; the host side is <DataDir>/proxy. The admin
	// API is a unix socket so no network, tile or LAN, can reach it.
	ProxyAdminDir = "/run/stackr-admin"
	// ProxyAdminListen is Caddy's admin listener inside the proxy.
	ProxyAdminListen = "unix/" + ProxyAdminDir + "/admin.sock"
	// DockerRanges are the private ranges docker hands out networks from;
	// the panel trusts X-Forwarded-For from them (the proxy's address) until
	// the install records the bridge subnet (Input.BridgeSubnet).
	// ponytail: all of RFC 1918, not the box's own pools; the panel listens
	// on the bridge gateway only, so a spoofer must already be on the box.
	DockerRanges = "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
	// File is the answers, kept in the data dir for re-run, upgrade, restore.
	File = "install.json"
	// KeyFile is the master key under the data dir; the panel archive
	// restores it to the same place.
	KeyFile = "keys/master.key"
)

// Input is everything about this install the containers vary on.
type Input struct {
	Root      string `json:"root"`       // tiles get names under it
	PanelHost string `json:"panel_host"` // the panel's own name, at or under Root
	HTTPS     bool   `json:"https"`
	Email     string `json:"email,omitempty"`   // ACME contact, HTTPS only
	Proxies   string `json:"proxies,omitempty"` // CDN ranges Caddy trusts, comma-joined
	DNS01     bool   `json:"dns01,omitempty"`   // Cloudflare DNS-01 (wildcards); the token lives on the proxy only
	HTTPPort  string `json:"http_port"`
	HTTPSPort string `json:"https_port,omitempty"`
	DataDir   string `json:"data_dir"`
	// Bind is the panel's listen address: the default bridge's gateway, the
	// address `host-gateway` names inside the proxy.
	Bind string `json:"bind"`
	// BridgeSubnet is the default bridge's subnet, where Caddy sits: the
	// only range the panel trusts X-Forwarded-For from. Empty (an older
	// install) keeps DockerRanges until the installer runs again.
	BridgeSubnet string `json:"bridge_subnet,omitempty"`
	// InstallID names this box's panel archives, so two installs on one
	// bucket never prune each other's. Empty (an older install) is "default".
	InstallID string `json:"install_id,omitempty"`
}

// AdminDir is the host directory holding the proxy's admin socket.
func (in Input) AdminDir() string { return in.DataDir + "/proxy" }

// BaseURL is how the operator reaches the panel.
func (in Input) BaseURL() string {
	switch {
	case in.HTTPS && in.HTTPSPort != "443":
		return "https://" + in.PanelHost + ":" + in.HTTPSPort
	case in.HTTPS:
		return "https://" + in.PanelHost
	case in.HTTPPort != "80":
		return "http://" + in.PanelHost + ":" + in.HTTPPort
	}
	return "http://" + in.PanelHost
}

// Image maps a release version (v0.1.1 or 0.1.1) to its image; CI tags
// images without the v.
func Image(version string) string { return Repo + ":" + strings.TrimPrefix(version, "v") }

// Container is one container, rendered as `docker run` args by the
// installer and converted to the Docker wrapper's spec by the panel.
type Container struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Cmd         []string          `json:"cmd,omitempty"`
	Env         []string          `json:"env"`
	Labels      map[string]string `json:"labels"`
	Volumes     []string          `json:"volumes"`
	Ports       []string          `json:"ports,omitempty"` // docker -p syntax
	ExtraHosts  []string          `json:"extra_hosts,omitempty"`
	HostNetwork bool              `json:"host_network,omitempty"`
	CapAdd      []string          `json:"cap_add,omitempty"`
	Restart     string            `json:"restart"`
}

// Panel is stackrd itself: host networking and NET_ADMIN for the VIP rules,
// the Docker socket, the data dir at the same path inside.
func Panel(image string, in Input) Container {
	tls := "off"
	if in.HTTPS {
		tls = "on"
	}
	dns := ""
	if in.DNS01 {
		dns = "cloudflare"
	}
	env := []string{
		"BASE_URL=" + in.BaseURL(),
		"STACKR_TLS=" + tls,
		"PANEL_DOMAIN=" + in.PanelHost,
		"ROOT_DOMAIN=" + strings.TrimPrefix(in.Root, "*."), // tiles get names under it
		"ACME_EMAIL=" + in.Email,
		"CADDY_TRUSTED_PROXIES=" + in.Proxies,
		"DNS_PROVIDER=" + dns,
		"DATA_DIR=" + in.DataDir,
		"DATABASE_PATH=" + in.DataDir + "/stackr.db",
		"HOST=" + in.Bind,
		"PORT=" + PanelPort,
		"TRUSTED_PROXIES=" + cmp.Or(in.BridgeSubnet, DockerRanges),
		"STACKR_PROXY_ADMIN=unix://" + in.AdminDir() + "/admin.sock",
		"STACKR_IMAGE=" + image,
	}
	if in.InstallID != "" {
		env = append(env, "STACKR_INSTALL_ID="+in.InstallID)
	}
	return Container{
		Name:   PanelName,
		Image:  image,
		Env:    env,
		Labels: map[string]string{LabelRole: "panel"},
		Volumes: []string{
			"/var/run/docker.sock:/var/run/docker.sock",
			in.DataDir + ":" + in.DataDir,
		},
		HostNetwork: true,
		CapAdd:      []string{"NET_ADMIN"},
		Restart:     "unless-stopped",
	}
}

// Proxy is `stackrd proxy`: Caddy with its data and config dirs on the
// named volume, the admin API on a unix socket, `stackr` naming the panel.
func Proxy(image string, in Input, dnsToken string) Container {
	ports := []string{in.HTTPPort + ":80"}
	if in.HTTPS {
		ports = append(ports, in.HTTPSPort+":443")
	}
	env := []string{"XDG_DATA_HOME=/caddy/data", "XDG_CONFIG_HOME=/caddy/config"}
	if dnsToken != "" {
		env = append(env, DNSTokenEnv+"="+dnsToken)
	}
	c := Container{
		Name:       ProxyName,
		Image:      image,
		Cmd:        []string{"proxy"},
		Env:        env,
		Labels:     map[string]string{LabelRole: "proxy"},
		Volumes:    []string{ProxyVolume + ":/caddy", in.AdminDir() + ":" + ProxyAdminDir},
		Ports:      ports,
		ExtraHosts: []string{PanelName + ":host-gateway"},
		Restart:    "unless-stopped",
	}
	c.Labels[LabelSpec] = c.Hash()
	return c
}

// Hash fingerprints everything the container is made from (image, cmd, env,
// mounts, ports, hosts, labels but its own stamp). Equal hash = nothing to
// recreate.
func (c Container) Hash() string {
	l := maps.Clone(c.Labels)
	delete(l, LabelSpec)
	c.Labels = l
	b, _ := json.Marshal(c) // plain strings and maps: cannot fail
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// TokenFrom finds the DNS-01 token in a container's env, "" when none.
func TokenFrom(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, DNSTokenEnv+"="); ok {
			return v
		}
	}
	return ""
}

// RunArgs is the `docker run` for c, flags in a fixed order.
func (c Container) RunArgs() []string {
	a := []string{"run", "-d", "--name", c.Name, "--restart", c.Restart}
	if c.HostNetwork {
		a = append(a, "--network", "host")
	}
	for _, v := range c.CapAdd {
		a = append(a, "--cap-add", v)
	}
	for _, k := range slices.Sorted(maps.Keys(c.Labels)) {
		a = append(a, "--label", k+"="+c.Labels[k])
	}
	for _, v := range c.Volumes {
		a = append(a, "-v", v)
	}
	for _, v := range c.Ports {
		a = append(a, "-p", v)
	}
	for _, v := range c.ExtraHosts {
		a = append(a, "--add-host", v)
	}
	for _, v := range c.Env {
		a = append(a, "-e", v)
	}
	return append(append(a, c.Image), c.Cmd...)
}

// Load reads the answers the installer saved in dataDir.
func Load(dataDir string) (Input, error) {
	var in Input
	b, err := os.ReadFile(filepath.Join(dataDir, File))
	if err != nil {
		return in, err
	}
	return in, json.Unmarshal(b, &in)
}

// Save writes the answers into in.DataDir.
func Save(in Input) error {
	b, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(in.DataDir, File), append(b, '\n'), 0o600)
}

// CleanHost takes what people paste: a URL with scheme, path, port, a
// trailing dot, capitals.
func CleanHost(s string) string {
	h := strings.ToLower(strings.TrimSpace(s))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "http://"), "https://")
	h, _, _ = strings.Cut(h, "/")
	h, _, _ = strings.Cut(h, ":")
	return strings.TrimSuffix(h, ".")
}

const hostChars = "abcdefghijklmnopqrstuvwxyz0123456789-"

// CheckRoot accepts a domain name Let's Encrypt can issue for, cleaned. A
// leading "*." is allowed. One grammar for the installer and the panel's
// own domain form.
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
