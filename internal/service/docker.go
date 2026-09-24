package service

import (
	"context"
	"io"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// Docker is everything the service tree asks of the daemon. The real client
// (service/internal/docker) and the test fake (service/internal/dockerfake)
// both satisfy it; WithDocker swaps one for the other. Leaves that need a
// slice of it declare their own smaller interface.
type Docker interface {
	// Containers.
	Run(ctx context.Context, spec docker.ContainerSpec) (id string, err error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Restart(ctx context.Context, id string) error
	StopRemove(ctx context.Context, id string) error
	Pause(ctx context.Context, id string) error
	Unpause(ctx context.Context, id string) error
	List(ctx context.Context, labels map[string]string) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (docker.Detail, error)

	// Networks.
	EnsureNetwork(ctx context.Context, name string, labels map[string]string) error
	RemoveNetwork(ctx context.Context, name string) error
	ListNetworks(ctx context.Context, labels map[string]string) ([]string, error)
	Connect(ctx context.Context, network, containerID string, aliases []string) error
	Disconnect(ctx context.Context, network, containerID string) error
	NetworkMembers(ctx context.Context, network string) ([]string, error)
	MemberAddr(ctx context.Context, network, containerID string) (ip, cidr string, err error)

	// Volumes.
	CreateVolume(ctx context.Context, name, driver string, opts, labels map[string]string) error
	RemoveVolume(ctx context.Context, name string) error
	InspectVolume(ctx context.Context, name string) (docker.VolumeInfo, error)
	ListVolumes(ctx context.Context, labels map[string]string) ([]docker.VolumeInfo, error)
	EnsureTool(ctx context.Context) error
	TarVolume(ctx context.Context, name string, w io.Writer, live bool) error
	UntarVolume(ctx context.Context, name string, r io.Reader) error

	// Images and builds.
	Pull(ctx context.Context, ref, auth string, log io.Writer) error
	LocalDigest(ctx context.Context, ref string) (string, error)
	Tag(ctx context.Context, src, dst string) error
	RemoveImage(ctx context.Context, ref string) error
	ListImages(ctx context.Context, labels map[string]string) ([]docker.Image, error)
	PruneImages(ctx context.Context, labels map[string]string, keep []string) (removed []string, err error)
	EnsureBuilder(ctx context.Context, name string, memMB int) error
	Build(ctx context.Context, builder, dir, dockerfile, tag string, buildArgs, labels map[string]string, log io.Writer) (imageID string, err error)

	// Logs and exec.
	Logs(ctx context.Context, id string, tail int) (string, error)
	StreamLogs(ctx context.Context, id string, tail int) (lines <-chan string, stop func(), err error)
	Exec(ctx context.Context, id string, cmd []string) (string, error)
	ExecStream(ctx context.Context, id string, cmd []string, stdin io.Reader) (out io.Reader, wait func() error, err error)
}

var _ Docker = (*docker.Client)(nil)

// Type aliases so callers outside the service tree can build and read specs.
type (
	ContainerSpec = docker.ContainerSpec
	Container     = docker.Container
)
