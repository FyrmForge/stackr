package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// linkDir is the per-repo directory holding the stack/environment binding,
// mirroring Railway's ./.railway. It is machine-local, gitignore it.
const linkDir = ".stackr"
const linkFile = "link.json"

// Link binds the current repo directory to a stack and environment, so verbs
// resolve their target from cwd instead of flags.
type Link struct {
	Stack     string `json:"stack"`      // stack id
	StackName string `json:"stack_name"` // cached for display
	Env       string `json:"env"`        // environment id
	EnvName   string `json:"env_name"`   // cached for display
	App       string `json:"app"`        // default app id
	AppName   string `json:"app_name"`   // cached for display
}

// FindLink walks up from cwd looking for the nearest .stackr/link.json (like
// git finding .git). Returns the link and the repo root that holds it.
// os.ErrNotExist if no link is found up to the filesystem root.
func FindLink() (Link, string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return Link{}, "", err
	}
	for {
		p := filepath.Join(dir, linkDir, linkFile)
		if b, err := os.ReadFile(p); err == nil {
			var l Link
			if err := json.Unmarshal(b, &l); err != nil {
				return Link{}, "", err
			}
			if l.Stack == "" {
				// Written before "project" was renamed to "stack". Read the old
				// keys rather than reporting the directory as unlinked, which is
				// what a plain unmarshal would silently do. The next SaveLink
				// writes the current names.
				var legacy struct {
					Stack     string `json:"project"`
					StackName string `json:"project_name"`
				}
				if json.Unmarshal(b, &legacy) == nil {
					l.Stack, l.StackName = legacy.Stack, legacy.StackName
				}
			}
			return l, dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached root
			return Link{}, "", os.ErrNotExist
		}
		dir = parent
	}
}

// SaveLink writes the link into ./.stackr/link.json in the given directory.
func SaveLink(dir string, l Link) error {
	d := filepath.Join(dir, linkDir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d, linkFile), b, 0o644)
}

// RemoveLink deletes the nearest link file. os.ErrNotExist if none is linked.
func RemoveLink() error {
	_, dir, err := FindLink()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, linkDir, linkFile))
}
