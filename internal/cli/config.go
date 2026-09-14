// Package cli implements the `stackr` command-line client.
package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config is the persisted CLI credential: which server, which key.
type Config struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

// ErrNotLoggedIn is returned by Load when no config file exists yet.
var ErrNotLoggedIn = errors.New("not logged in; run `stackr login <url>`")

// configPath resolves $XDG_CONFIG_HOME/stackr/config.json, falling back to
// ~/.config per the XDG base-dir spec.
func configPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "stackr", "config.json"), nil
}

// Load reads the saved credential. Missing file → ErrNotLoggedIn.
func Load() (Config, error) {
	p, err := configPath()
	if err != nil {
		return Config{}, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNotLoggedIn
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save writes the credential with owner-only permissions (0600 file, 0700 dir).
func Save(c Config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// Clear removes the saved credential. Absent file is not an error.
func Clear() error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
