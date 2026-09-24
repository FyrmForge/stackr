package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Local is a destination directory, e.g. <dataDir>/backups/archives.
type Local struct{ Dir string }

const tmpPrefix = ".tmp-"

// file maps a key into Dir. Cleaning it as an absolute path first drops every
// "..", so no key reaches outside Dir.
func (l Local) file(key string) (string, error) {
	clean := path.Clean("/" + key)
	if clean == "/" || strings.HasPrefix(path.Base(clean), tmpPrefix) {
		return "", fmt.Errorf("backup destination: bad key %q", key)
	}
	return filepath.Join(l.Dir, filepath.FromSlash(clean)), nil
}

// Put writes to a temp file beside the target and renames it into place, so
// a crash never leaves a half archive under a real key.
func (l Local) Put(_ context.Context, key string, body io.ReadSeeker) error {
	dst, err := l.file(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), tmpPrefix+"*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() // no-op after the rename
	_, err = io.Copy(f, body)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), dst)
}

func (l Local) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.file(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (l Local) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(l.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == l.Dir && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll // nothing stored yet
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		rel, err := filepath.Rel(l.Dir, p)
		if err != nil {
			return err
		}
		if key := filepath.ToSlash(rel); strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	slices.Sort(keys)
	return keys, err
}

func (l Local) Delete(_ context.Context, key string) error {
	p, err := l.file(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
