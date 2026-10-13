// Package docker wraps the Docker daemon. It is the only place the Docker SDK
// is imported, and no SDK type appears in a signature here. It takes resolved
// specs and returns Docker's own words: it reads no row, decides no status and
// chooses no default of its own.
package docker

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/client"
)

// LabelManaged is the only label this package owns: every container it
// creates carries it, so a sweep can find its own droppings.
const LabelManaged = "stackr.managed"

// ContainerSpec is a resolved container: every value is final, nothing here
// is looked up, defaulted from a row, or validated against a domain rule.
type ContainerSpec struct {
	Name    string
	Image   string            // by digest when the caller pins one
	Cmd     []string          // override image CMD (nil = image default)
	Env     []string          // KEY=VALUE
	Labels  map[string]string //
	Volumes []string          // "name-or-hostpath:/container/path[:ro]"
	Ports   map[string]string // host[/udp] -> container[/udp] (published)
	// AnyIP publishes Ports on every host address, IPv6 included (the
	// proxy); unset binds IPv4 only.
	AnyIP    bool
	Networks []NetAttach // every network, joined at create
	// HostNetwork runs in the host's network namespace; Networks and Ports
	// are ignored (the proxy and stackrd itself).
	HostNetwork bool
	CapAdd      []string // e.g. NET_ADMIN
	ExtraHosts  []string // "name:ip" or "name:host-gateway"
	CapDrop     []string // e.g. NET_RAW
	CPULimit    float64  // cores, 0 = unlimited
	MemLimitMB  int      // MB, 0 = unlimited

	User       string   // "uid[:gid]" ("" = image default)
	ShmSizeMB  int      // 0 = docker default (64MB)
	Privileged bool     // the caller gates who may set it
	Devices    []Device // already parsed
	// Restart is docker's word: no | always | on-failure | unless-stopped.
	// "" = docker's default (no).
	// extract: dropped RestartAlways bool + NormalizeRestart, belongs in leaf/tile.
	Restart string

	// Docker-native HEALTHCHECK, run as CMD-SHELL. Empty HealthCmd = none.
	// Zero values are docker's defaults.
	// extract: dropped the 5s interval default (docker's is 30s, the deploy
	// gate waits on the first check), belongs in flow/deploy spec build.
	HealthCmd          string
	HealthIntervalS    int
	HealthTimeoutS     int
	HealthRetries      int
	HealthStartPeriodS int
}

// HostInfo is the daemon host's capacity; 0 = not reported.
type HostInfo struct {
	CPUs     int
	MemBytes int64
	// Arch is the daemon's Architecture as Docker reports it ("x86_64",
	// "aarch64"); "" = not reported.
	Arch string
}

// NetAttach is one network the container joins, with its DNS aliases there.
// IPv4 pins the address ("" = Docker picks one from the dynamic range).
type NetAttach struct {
	Name    string
	Aliases []string
	IPv4    string
}

type Device struct{ Host, Container, Perms string }

// Container is one row of a list.
type Container struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Image string `json:"image"`
	State string `json:"state"`
	// Health is docker's own word (healthy | unhealthy | starting), "" when
	// the container declares no HEALTHCHECK.
	Health string            `json:"health"`
	Labels map[string]string `json:"labels"`
	IPs    []string          `json:"ips"`
	// Nets is the address per network name, running containers only.
	Nets map[string]string `json:"-"`
	// HostNetwork: the container runs in the host's network namespace.
	HostNetwork bool `json:"-"`
}

// Detail is the curated inspect: everything a health gate reads comes off
// this one call.
type Detail struct {
	ID, Name, Image, State, Started string
	Running                         bool
	Health                          string
	RestartCount                    int
	// HealthInterval, HealthStartPeriod and HealthRetries are the container's
	// effective HEALTHCHECK timings (the image's own when the spec set none);
	// zero = docker's default.
	HealthInterval, HealthStartPeriod time.Duration
	HealthRetries                     int
	Ports, Mounts                     []string
	Env                               []string          // KEY=VALUE, as created
	Networks                          map[string]string // network name -> IP
	HostNetwork                       bool              // runs in the host's network namespace
}

type VolumeInfo struct {
	Name, Driver, Created, Mountpoint string
	Labels                            map[string]string
	SizeBytes                         int64    // -1 = unknown
	UsedBy                            []string // running containers mounting it
	HeldBy                            []string // stopped containers mounting it
}

// Image is one local image.
type Image struct {
	ID     string
	Tags   []string // repo:tag
	Labels map[string]string
	Size   int64
}

// ErrNotFound: the container, network, volume or image does not exist.
var ErrNotFound = errors.New("docker: no such object")

// ErrStillRunning: an exec's output ended while the command was still running,
// so its exit code (0) means nothing.
var ErrStillRunning = errors.New("docker: command still running when its output ended")

// ErrNotRunning: an exec was refused because the container is not running.
var ErrNotRunning = errors.New("docker: container is not running")

// ErrUnreachable: the daemon did not answer. The SDK has its own error for
// it; this one is for stand-ins (the fake host's blip).
var ErrUnreachable = errors.New("docker: the daemon is not reachable")

// Unreachable reports whether err is the daemon not answering, from any call.
func Unreachable(err error) bool {
	return errors.Is(err, ErrUnreachable) || client.IsErrConnectionFailed(err)
}

// ExitError is a command (exec or tool container) that exited non-zero.
type ExitError struct {
	Code   int
	Stderr string
}

func (e ExitError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return fmt.Sprintf("exit status %d: %s", e.Code, e.Stderr)
}

// wrap turns the SDK's not-found into ErrNotFound; everything else passes.
func wrap(err error) error {
	if err != nil && cerrdefs.IsNotFound(err) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

// Client is the daemon wrapper.
type Client struct {
	cli  *client.Client
	tool string    // the tool containers' image, "" = ToolImage
	px   *cliProxy // a worker's: how the docker CLI reaches it; nil = local
	// remote: another host's Docker, so this process's own interfaces say
	// nothing about it.
	remote bool
}

// SetTool makes the tool containers run ref instead of ToolImage: a worker
// takes stackr's own helper from the cluster registry, not Docker Hub.
func (d *Client) SetTool(ref string) { d.tool = ref }

func (d *Client) toolImage() string {
	if d.tool != "" {
		return d.tool
	}
	return ToolImage
}

func New() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// NewRemote dials a worker node's proxied Docker API: host is "tcp://addr"
// and hc carries the mTLS transport (workerclient.DockerOptions).
func NewRemote(host string, hc *http.Client) (*Client, error) {
	// Read the TLS config first: the SDK wraps hc.Transport (tracing) in place.
	var px *cliProxy
	if tr, ok := hc.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
		px = newCLIProxy(strings.TrimPrefix(host, "tcp://"), tr.TLSClientConfig)
	}
	cli, err := client.NewClientWithOpts(client.WithHost(host), client.WithHTTPClient(hc), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client for %s: %w", host, err)
	}
	return &Client{cli: cli, px: px, remote: true}, nil
}
