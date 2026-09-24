// Package docker wraps the Docker daemon. It is the only place the Docker SDK
// is imported, and no SDK type appears in a signature here. It takes resolved
// specs and returns Docker's own words: it reads no row, decides no status and
// chooses no default of its own.
package docker

import (
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/client"
)

// LabelManaged is the only label this package owns: every container it
// creates carries it, so a sweep can find its own droppings.
const LabelManaged = "stackr.managed"

// ContainerSpec is a resolved container: every value is final, nothing here
// is looked up, defaulted from a row, or validated against a domain rule.
type ContainerSpec struct {
	Name     string
	Image    string            // by digest when the caller pins one
	Cmd      []string          // override image CMD (nil = image default)
	Env      []string          // KEY=VALUE
	Labels   map[string]string //
	Volumes  []string          // "name-or-hostpath:/container/path[:ro]"
	Ports    map[string]string // hostPort -> containerPort (published)
	Networks []NetAttach       // every network, joined at create
	// HostNetwork runs in the host's network namespace; Networks and Ports
	// are ignored (the proxy and stackrd itself).
	HostNetwork bool
	CapAdd      []string // e.g. NET_ADMIN
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

// NetAttach is one network the container joins, with its DNS aliases there.
type NetAttach struct {
	Name    string
	Aliases []string
}

type Device struct{ Host, Container, Perms string }

// Container is one row of a list.
type Container struct {
	ID, Name, Image, State string
	// Health is docker's own word (healthy | unhealthy | starting), "" when
	// the container declares no HEALTHCHECK.
	Health string
	Labels map[string]string
	IPs    []string
}

// Detail is the curated inspect: everything a health gate reads comes off
// this one call.
type Detail struct {
	ID, Name, Image, State, Started string
	Running                         bool
	Health                          string
	RestartCount                    int
	Ports, Mounts                   []string
	Networks                        map[string]string // network name -> IP
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
}

// ErrNotFound: the container, network, volume or image does not exist.
var ErrNotFound = errors.New("docker: no such object")

// ErrStillRunning: an exec's output ended while the command was still running,
// so its exit code (0) means nothing.
var ErrStillRunning = errors.New("docker: command still running when its output ended")

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
type Client struct{ cli *client.Client }

func New() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}
