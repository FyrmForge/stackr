package docker

import (
	"context"
	"errors"
	"io"
)

// ErrNotImplemented is every stub answer.
var ErrNotImplemented = errors.New("docker: not implemented")

// ponytail: compile-only stubs, task 3 replaces them.

func (*Client) Pull(context.Context, string, string, io.Writer) error { return ErrNotImplemented }
func (*Client) LocalDigest(context.Context, string) (string, error)   { return "", ErrNotImplemented }
func (*Client) Tag(context.Context, string, string) error             { return ErrNotImplemented }
func (*Client) RemoveImage(context.Context, string) error             { return ErrNotImplemented }
func (*Client) EnsureBuilder(context.Context, string, int) error      { return ErrNotImplemented }
func (*Client) Build(context.Context, string, string, string, string, map[string]string, map[string]string, io.Writer) error {
	return ErrNotImplemented
}
