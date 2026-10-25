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
	"strconv"
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
	// KeyNextFile is a master key waiting to replace KeyFile; a restore
	// removes a leftover one, it belongs to the box that was replaced.
	KeyNextFile = "keys/master.key.next"
	// RecoveryPubFile is the recovery key's public half (age X25519) and its
	// generation, two lines. This box's own, never in an archive: a restore on
	// a new box keeps the box's key. RecoveryPendingFile holds the private
	// half until an admin has saved it.
	RecoveryPubFile     = "keys/recovery.pub"
	RecoveryPendingFile = "keys/recovery.pending"
	// RestoredMarker is what stackr-install restore leaves in the data dir:
	// the panel's next boot redeploys the controller's tiles and removes it.
	// Never in an archive.
	RestoredMarker = "restored"
	// Cluster files under the data dir; the panel archive carries them so a
	// restored head is the same head (same CA, same WireGuard identity).
	ClusterCAKeyFile  = "keys/cluster-ca.key"
	ClusterCACertFile = "keys/cluster-ca.crt"
	WGKeyFile         = "keys/wg.key"
	// The previous root and the cross cert, kept while a CA rollover runs:
	// without them a restore mid-rollover finishes it and cuts off workers
	// still on the old root.
	ClusterCAPrevCertFile  = "keys/cluster-ca.prev.crt"
	ClusterCACrossCertFile = "keys/cluster-ca.cross.crt"
	// The controller's own cert and key, so a restore keeps them.
	ControllerKeyFile  = "keys/controller.key"
	ControllerCertFile = "keys/controller.crt"
	// The controller's previous cert and key, kept from a renewal until a
	// revoke or break glass: recovery answers carry its signature too.
	ControllerPrevKeyFile  = "keys/controller-prev.key"
	ControllerPrevCertFile = "keys/controller-prev.crt"
	// DBFile, VersionMember and CaddyMember are the other archive names.
	DBFile        = "stackr.db"
	VersionMember = "VERSION"
	// RecoveryMember stamps an archive with the generation of the recovery
	// key it is encrypted to.
	RecoveryMember = "RECOVERY"
	CaddyMember    = "caddy.tar.gz"
	// MasterGenMember stamps an archive with the generation of the master
	// key it carries.
	MasterGenMember = "MASTERGEN"
)

// Member is one entry of the panel archive.
type Member struct {
	Name     string
	Mode     int64
	Optional bool // may be missing from an archive (and from the box at backup)
	// OnDisk: the file lives at Name under the data dir, and restore moves
	// it back there. VERSION and the proxy volume tar do not.
	OnDisk bool
}

// PanelState is everything the panel needs to come back, in archive order.
// PanelBackup writes exactly these members and `stackr-install restore`
// accepts exactly these; any other entry is refused. A new key file is added
// here and nowhere else. Later: registry storage.
var PanelState = []Member{
	{Name: DBFile, Mode: 0o600, OnDisk: true},
	{Name: KeyFile, Mode: 0o600, OnDisk: true},
	{Name: VersionMember, Mode: 0o644},
	{Name: RecoveryMember, Mode: 0o644},
	{Name: MasterGenMember, Mode: 0o644},
	{Name: ClusterCAKeyFile, Mode: 0o600, Optional: true, OnDisk: true},
	{Name: ClusterCACertFile, Mode: 0o644, Optional: true, OnDisk: true},
	{Name: ClusterCAPrevCertFile, Mode: 0o644, Optional: true, OnDisk: true},
	{Name: ClusterCACrossCertFile, Mode: 0o644, Optional: true, OnDisk: true},
	{Name: WGKeyFile, Mode: 0o600, Optional: true, OnDisk: true},
	{Name: ControllerKeyFile, Mode: 0o600, Optional: true, OnDisk: true},
	{Name: ControllerCertFile, Mode: 0o644, Optional: true, OnDisk: true},
	{Name: ControllerPrevKeyFile, Mode: 0o600, Optional: true, OnDisk: true},
	{Name: ControllerPrevCertFile, Mode: 0o644, Optional: true, OnDisk: true},
	{Name: CaddyMember, Mode: 0o600, Optional: true},
}

// LoadRecovery reads the recovery public key and its generation; pub is ""
// when the box has none yet.
func LoadRecovery(dataDir string) (pub string, gen int, err error) {
	b, err := os.ReadFile(filepath.Join(dataDir, RecoveryPubFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	pub, n, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	gen, err = strconv.Atoi(strings.TrimSpace(n))
	if err != nil || gen < 1 || !strings.HasPrefix(pub, "age1") {
		return "", 0, fmt.Errorf("%s is not a recovery public key and generation", RecoveryPubFile)
	}
	return pub, gen, nil
}

// SaveRecovery writes the public key and generation (0600, atomically).
func SaveRecovery(dataDir, pub string, gen int) error {
	return WriteKeyFile(filepath.Join(dataDir, RecoveryPubFile), []byte(pub+"\n"+strconv.Itoa(gen)+"\n"))
}

// WriteKeyFile writes a key file 0600 through a rename, so a crash leaves
// the old file or the new one, never half of one.
func WriteKeyFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Input is everything about this install the containers vary on.
type Input struct {
	Root      string `json:"root"`       // tiles get names under it
	PanelHost string `json:"panel_host"` // the panel's own name, at or under Root
	HTTPS     bool   `json:"https"`
	Email     string `json:"email,omitempty"`   // ACME contact, HTTPS only
	Proxies   string `json:"proxies,omitempty"` // CDN ranges Caddy trusts, comma-joined
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
	// WGPort is the controller's mesh UDP port; 0 is mesh.DefaultPort.
	WGPort int `json:"wg_port,omitempty"`
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

// SwapSpecEnv carries the new panel's spec into `stackrd upgrade-swap`. A
// wire contract: an older image's helper reads the same name.
const SwapSpecEnv = "STACKR_SWAP_SPEC"

// SwapSpec is the SwapSpecEnv value `stackrd upgrade-swap` reads, for the
// installer's upgrade: the panel at version as the panel's own Update hands
// it over, named after the release so the old container keeps its name until
// the new one passes. The field names are the Docker wrapper's (it has no
// json tags); cmd/stackrd's spec test holds the two equal.
func SwapSpec(version string, in Input) (string, error) {
	c := Panel(Image(version), in)
	b, err := json.Marshal(struct {
		Name, Image string
		Cmd, Env    []string
		Labels      map[string]string
		Volumes     []string
		HostNetwork bool
		CapAdd      []string
		Restart     string
	}{PanelName + "-" + strings.TrimPrefix(version, "v"), c.Image, c.Cmd, c.Env, c.Labels, c.Volumes, c.HostNetwork, c.CapAdd, c.Restart})
	return string(b), err
}

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
	Network     string            `json:"network,omitempty"` // a docker network mode ("none"); ignored with HostNetwork
	CapAdd      []string          `json:"cap_add,omitempty"`
	LogOpts     []string          `json:"log_opts,omitempty"`  // docker --log-opt values
	MemoryMB    int               `json:"memory_mb,omitempty"` // docker --memory; 0 = none
	Restart     string            `json:"restart"`
}

// logCap is the json-file cap on every container the installer makes: the
// daemon's default keeps logs forever.
var logCap = []string{"max-size=10m", "max-file=3"}

// Panel is stackrd itself: host networking and NET_ADMIN for the VIP rules,
// the Docker socket, the data dir at the same path inside.
func Panel(image string, in Input) Container {
	tls := "off"
	if in.HTTPS {
		tls = "on"
	}
	env := []string{
		"BASE_URL=" + in.BaseURL(),
		"STACKR_TLS=" + tls,
		"PANEL_DOMAIN=" + in.PanelHost,
		"ROOT_DOMAIN=" + strings.TrimPrefix(in.Root, "*."), // tiles get names under it
		"ACME_EMAIL=" + in.Email,
		"CADDY_TRUSTED_PROXIES=" + in.Proxies,
		"DATA_DIR=" + in.DataDir,
		"DATABASE_PATH=" + in.DataDir + "/stackr.db",
		"HOST=" + in.Bind,
		"PORT=" + PanelPort,
		"TRUSTED_PROXIES=" + cmp.Or(in.BridgeSubnet, DockerRanges),
		"STACKR_PROXY_ADMIN=unix://" + in.AdminDir() + "/admin.sock",
		"STACKR_IMAGE=" + image,
		"STACKR_HOST_ROOT=" + HostRoot,
	}
	if in.InstallID != "" {
		env = append(env, "STACKR_INSTALL_ID="+in.InstallID)
	}
	if in.WGPort != 0 {
		env = append(env, "STACKR_MESH_WG_PORT="+strconv.Itoa(in.WGPort))
	}
	return Container{
		Name:   PanelName,
		Image:  image,
		Env:    env,
		Labels: map[string]string{LabelRole: "panel"},
		Volumes: []string{
			"/var/run/docker.sock:/var/run/docker.sock",
			in.DataDir + ":" + in.DataDir,
			// Read-only host views for container and host metrics: the
			// panel's own cgroup namespace and pid namespace hide them.
			"/sys/fs/cgroup:" + HostRoot + "/sys/fs/cgroup:ro",
			"/proc:" + HostRoot + "/proc:ro",
		},
		HostNetwork: true,
		CapAdd:      []string{"NET_ADMIN"},
		LogOpts:     logCap,
		Restart:     "unless-stopped",
	}
}

// Proxy is `stackrd proxy`: Caddy with its data and config dirs on the
// named volume, the admin API on a unix socket, `stackr` naming the panel.
func Proxy(image string, in Input) Container {
	ports := []string{in.HTTPPort + ":80"}
	if in.HTTPS {
		ports = append(ports, in.HTTPSPort+":443")
	}
	env := []string{"XDG_DATA_HOME=/caddy/data", "XDG_CONFIG_HOME=/caddy/config"}
	c := Container{
		Name:       ProxyName,
		Image:      image,
		Cmd:        []string{"proxy"},
		Env:        env,
		Labels:     map[string]string{LabelRole: "proxy"},
		Volumes:    []string{ProxyVolume + ":/caddy", in.AdminDir() + ":" + ProxyAdminDir},
		Ports:      ports,
		ExtraHosts: []string{PanelName + ":host-gateway"},
		LogOpts:    logCap,
		Restart:    "unless-stopped",
	}
	c.Labels[LabelSpec] = c.Hash()
	return c
}

// WorkerProxyAdminDir is the host dir of an ingress worker's Caddy admin
// socket; `stackrd worker` passes its admin API through from there.
const WorkerProxyAdminDir = "/var/lib/stackr-worker/proxy"

// NodeProxy is the proxy on an ingress worker: Proxy's Caddy on the host
// network, so it binds 80 and 443 itself and reaches local tiles and other
// nodes' VIPs (over stackr0, from its transit address) without NAT. It serves no
// panel. Certificates live on its own stackr-caddy volume.
func NodeProxy(image string) Container {
	env := []string{"XDG_DATA_HOME=/caddy/data", "XDG_CONFIG_HOME=/caddy/config"}
	c := Container{
		Name:        ProxyName,
		Image:       image,
		Cmd:         []string{"proxy"},
		Env:         env,
		Labels:      map[string]string{LabelRole: "proxy"},
		Volumes:     []string{ProxyVolume + ":/caddy", WorkerProxyAdminDir + ":" + ProxyAdminDir},
		HostNetwork: true,
		LogOpts:     logCap,
		Restart:     "unless-stopped",
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

// RunArgs is the `docker run` for c, flags in a fixed order.
func (c Container) RunArgs() []string {
	a := []string{"run", "-d", "--name", c.Name, "--restart", c.Restart}
	if c.HostNetwork {
		a = append(a, "--network", "host")
	} else if c.Network != "" {
		a = append(a, "--network", c.Network)
	}
	for _, v := range c.CapAdd {
		a = append(a, "--cap-add", v)
	}
	for _, k := range slices.Sorted(maps.Keys(c.Labels)) {
		a = append(a, "--label", k+"="+c.Labels[k])
	}
	for _, v := range c.LogOpts {
		a = append(a, "--log-opt", v)
	}
	if c.MemoryMB > 0 {
		a = append(a, "--memory", strconv.Itoa(c.MemoryMB)+"m")
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
