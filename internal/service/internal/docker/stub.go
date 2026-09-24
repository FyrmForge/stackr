package docker

import (
	"context"
	"errors"
	"io"
)

// ErrNotImplemented is every stub answer.
var ErrNotImplemented = errors.New("docker: not implemented")

// ponytail: compile-only stubs, tasks 2 and 3 replace them.

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
