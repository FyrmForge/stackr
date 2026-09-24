// Package docker wraps the Docker daemon. Step 1 ships the types and a stub
// that satisfies service.Docker; step 2 fills it in. No Docker SDK type ever
// appears in a signature here.
package docker

import (
	"context"
	"errors"
	"io"
)

// ContainerSpec is a resolved container: every value is final, nothing here
// is looked up, defaulted from a row, or validated against a domain rule.
type ContainerSpec struct {
	Name        string
	Image       string
	Cmd         []string // override image CMD (nil = image default)
	Env         []string // KEY=VALUE
	Labels      map[string]string
	Volumes     []string          // "name-or-hostpath:/container/path"
	Ports       map[string]string // hostPort -> containerPort (published)
	Aliases     []string          // stable DNS names on the network
	NetworkName string
	CPULimit    float64 // cores, 0 = unlimited
	MemLimitMB  int     // MB, 0 = unlimited

	User          string   // "uid[:gid]" ("" = image default)
	ShmSizeMB     int      // 0 = docker default (64MB)
	Privileged    bool     // the caller gates who may set it
	Devices       []Device // already parsed
	RestartAlways bool     // else unless-stopped

	// Docker-native HEALTHCHECK, run as CMD-SHELL. Empty HealthCmd = none.
	HealthCmd          string
	HealthIntervalS    int
	HealthTimeoutS     int
	HealthRetries      int
	HealthStartPeriodS int
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

// Detail is the curated inspect.
type Detail struct {
	ID, Name, Image, State, Started string
	Health                          string
	RestartCount                    int
	Ports, Mounts, Networks         []string
}

type VolumeInfo struct {
	Name, Driver, Created, Mountpoint string
	SizeBytes                         int64    // -1 = unknown
	UsedBy                            []string // running containers mounting it
	HeldBy                            []string // stopped containers mounting it
}

// ErrNotImplemented is every stub answer.
var ErrNotImplemented = errors.New("docker: not implemented")

// Client is the daemon wrapper.
// ponytail: compile-only stub, every call fails; step 2 replaces the bodies.
type Client struct{}

func New() (*Client, error) { return &Client{}, nil }

func (*Client) Run(context.Context, ContainerSpec) (string, error) { return "", ErrNotImplemented }
func (*Client) Start(context.Context, string) error                { return ErrNotImplemented }
func (*Client) Stop(context.Context, string) error                 { return ErrNotImplemented }
func (*Client) Restart(context.Context, string) error              { return ErrNotImplemented }
func (*Client) StopRemove(context.Context, string) error           { return ErrNotImplemented }
func (*Client) Pause(context.Context, string) error                { return ErrNotImplemented }
func (*Client) Unpause(context.Context, string) error              { return ErrNotImplemented }
func (*Client) List(context.Context, map[string]string) ([]Container, error) {
	return nil, ErrNotImplemented
}
func (*Client) Inspect(context.Context, string) (Detail, error) { return Detail{}, ErrNotImplemented }

func (*Client) EnsureNetwork(context.Context, string) error { return ErrNotImplemented }
func (*Client) RemoveNetwork(context.Context, string) error { return ErrNotImplemented }
func (*Client) Connect(context.Context, string, string, []string) error {
	return ErrNotImplemented
}
func (*Client) Disconnect(context.Context, string, string) error { return ErrNotImplemented }
func (*Client) NetworkMembers(context.Context, string) ([]string, error) {
	return nil, ErrNotImplemented
}
func (*Client) MemberAddr(context.Context, string, string) (string, string, error) {
	return "", "", ErrNotImplemented
}

func (*Client) CreateVolume(context.Context, string, string, map[string]string) error {
	return ErrNotImplemented
}
func (*Client) RemoveVolume(context.Context, string) error { return ErrNotImplemented }
func (*Client) InspectVolume(context.Context, string) (VolumeInfo, error) {
	return VolumeInfo{}, ErrNotImplemented
}
func (*Client) ListVolumes(context.Context) ([]VolumeInfo, error) { return nil, ErrNotImplemented }
func (*Client) TarVolume(context.Context, string, io.Writer, bool) error {
	return ErrNotImplemented
}
func (*Client) UntarVolume(context.Context, string, io.Reader) error { return ErrNotImplemented }

func (*Client) Pull(context.Context, string, string, io.Writer) error { return ErrNotImplemented }
func (*Client) LocalDigest(context.Context, string) (string, error)   { return "", ErrNotImplemented }
func (*Client) Tag(context.Context, string, string) error             { return ErrNotImplemented }
func (*Client) RemoveImage(context.Context, string) error             { return ErrNotImplemented }
func (*Client) EnsureBuilder(context.Context, string, int) error      { return ErrNotImplemented }
func (*Client) Build(context.Context, string, string, string, string, map[string]string, map[string]string, io.Writer) error {
	return ErrNotImplemented
}

func (*Client) Logs(context.Context, string, int) (string, error) { return "", ErrNotImplemented }
func (*Client) StreamLogs(context.Context, string, int) (<-chan string, func(), error) {
	return nil, nil, ErrNotImplemented
}
func (*Client) Exec(context.Context, string, []string) (string, error) {
	return "", ErrNotImplemented
}
func (*Client) ExecStream(context.Context, string, []string, io.Reader) (io.Reader, func() error, error) {
	return nil, nil, ErrNotImplemented
}
