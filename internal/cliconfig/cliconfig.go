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

// Save writes c to path, 0600, making its directory 0700.
func Save(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
