// Package s3 talks to backup destinations: a directory under the data dir
// (every install has one) and S3-compatible buckets. Keys in, bytes out; what
// a key means, which archives to keep and how they are encrypted is the
// caller's.
package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
)

// Destination is where backup archives live. Keys are slash-separated and
// timestamp-first, so List's lexical order is chronological order.
type Destination interface {
	// Put stores body under key, replacing any object there. A seeker
	// because S3 needs the length up front (the archive is a finished
	// scratch file by then).
	Put(ctx context.Context, key string, body io.ReadSeeker) error
	// Get opens key; ErrNotFound when there is none. The caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns every key under prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes key; a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

var ErrNotFound = errors.New("backup destination: no such object")

// Probe checks a destination by writing and deleting a small object.
func Probe(ctx context.Context, d Destination) error {
	const key = ".stackr-probe"
	if err := d.Put(ctx, key, bytes.NewReader([]byte("ok"))); err != nil {
		return err
	}
	return d.Delete(ctx, key)
}
