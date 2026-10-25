// Package s3 talks to backup destinations: a directory under the data dir
// (every install has one) and S3-compatible buckets. Keys in, bytes out; what
// a key means, which archives to keep and how they are encrypted is the
// caller's.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// Destination is where backup archives live. Keys are slash-separated and
// timestamp-first, so List's lexical order is chronological order.
type Destination interface {
	// Put stores body under key, replacing any object there. The length is
	// not known up front, so a live backup can stream straight in; a failed
	// Put leaves no object (and no stray multipart parts) behind.
	Put(ctx context.Context, key string, body io.Reader) error
	// Get opens key; ErrNotFound when there is none. The caller closes it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns every key under prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete removes key; a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

var ErrNotFound = errors.New("backup destination: no such object")

// Probe checks a destination by writing and deleting a small object. An
// S3 refusal comes back as its code and message alone.
func Probe(ctx context.Context, d Destination) error {
	const key = ".stackr-probe"
	if err := d.Put(ctx, key, bytes.NewReader([]byte("ok"))); err != nil {
		return short(err)
	}
	return short(d.Delete(ctx, key))
}

func short(err error) error {
	var api interface {
		ErrorCode() string
		ErrorMessage() string
	}
	if errors.As(err, &api) {
		return fmt.Errorf("%s: %s", api.ErrorCode(), api.ErrorMessage())
	}
	return err
}
