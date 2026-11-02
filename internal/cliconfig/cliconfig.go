// Package cliconfig is the stackr CLI's config file, shared with the
// installer, which writes the controller's own CLI login.
package cliconfig

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// Default names the profile a file without profiles holds.
const Default = "default"

// Config is the one file the CLI keeps, 0600. Server, Key, KeyID and Org are
// the current profile's; the others wait in Profiles.
type Config struct {
	Server string          `json:"server"`
	Key    string          `json:"key"`
	KeyID  string          `json:"key_id,omitempty"` // the key a browser login minted: logout revokes it
	Org    string          `json:"org"`
	Links  map[string]Link `json:"links,omitempty"` // directory -> link

	Profile  string             `json:"profile,omitempty"` // the current profile's name; "" is Default
	Profiles map[string]Profile `json:"profiles,omitempty"`
}

// Profile is a server login that is not the current one.
type Profile struct {
	Server string `json:"server"`
	Key    string `json:"key"`
	KeyID  string `json:"key_id,omitempty"`
	Org    string `json:"org"`
}

// Name is the current profile's name.
func (c Config) Name() string {
	if c.Profile == "" {
		return Default
	}
	return c.Profile
}

// Use returns c with profile name current and the old current stashed. A
// profile that does not exist comes back empty when create is set, else
// ok is false.
func (c Config) Use(name string, create bool) (Config, bool) {
	if name == "" || name == c.Name() {
		return c, true
	}
	p, ok := c.Profiles[name]
	if !ok && !create {
		return c, false
	}
	m := maps.Clone(c.Profiles)
	if m == nil {
		m = map[string]Profile{}
	}
	m[c.Name()] = Profile{c.Server, c.Key, c.KeyID, c.Org}
	delete(m, name)
	c.Server, c.Key, c.KeyID, c.Org = p.Server, p.Key, p.KeyID, p.Org
	c.Profile, c.Profiles = name, m
	if name == Default {
		c.Profile = ""
	}
	return c, true
}

// Names are every profile's name, current first.
func (c Config) Names() []string {
	return append([]string{c.Name()}, slices.Sorted(maps.Keys(c.Profiles))...)
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
