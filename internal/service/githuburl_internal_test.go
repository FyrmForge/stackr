package service

import (
	"os"
	"path/filepath"
	"testing"
)

// A bad GitHub dev URL fails New before the DB exists or anything runs.
func TestNewBadGitHubURLOpensNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	_, err := New(Config{DataDir: dir, SecretsKey: testKey, GitHubURL: "http://plain"})
	if err == nil {
		t.Fatal("http GitHub URL accepted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("data dir touched before the kinds were validated: %v", err)
	}
}
