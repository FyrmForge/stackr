// Package cliconfig is the stackr CLI's config file, shared with the
// installer, which writes the controller's own CLI login.
package cliconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the one file the CLI keeps, 0600.
type Config struct {
	Server string          `json:"server"`
	Key    string          `json:"key"`
	Org    string          `json:"org"`
	Links  map[string]Link `json:"links,omitempty"` // directory -> link
}

type Link struct {
	Stack string `json:"stack,omitempty"`
	Env   string `json:"env,omitempty"`
	Tile  string `json:"tile,omitempty"`
}

// Save writes c to path, 0600, making its directory 0700. A temp file
// renamed over path: a planted symlink is replaced, never written through,
// and an old file's looser mode does not survive.
func Save(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cli-*") // 0600
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
