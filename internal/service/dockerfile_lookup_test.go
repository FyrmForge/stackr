package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/git"
)

func TestDockerfileIn(t *testing.T) {
	put := func(dir string, rel ...string) {
		for _, p := range rel {
			f := filepath.Join(dir, p)
			if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, []byte("FROM scratch"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct {
		name     string
		files    []string
		explicit string
		want     string // repo-relative; "" = error
	}{
		{"context only", []string{"app/Dockerfile"}, "", "app/Dockerfile"},
		{"root only", []string{"Dockerfile"}, "", "Dockerfile"},
		{"both, context wins", []string{"app/Dockerfile", "Dockerfile"}, "", "app/Dockerfile"},
		{"explicit stays repo-relative", []string{"app/Dockerfile", "ci/Dockerfile.x"}, "ci/Dockerfile.x", "ci/Dockerfile.x"},
		{"neither", nil, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dir, _ = filepath.EvalSymlinks(dir)
			put(dir, c.files...)
			got, err := dockerfileIn(git.Repo{Dir: dir}, "app", c.explicit)
			if c.want == "" {
				if err == nil || !strings.Contains(err.Error(), "app/Dockerfile") || !strings.Contains(err.Error(), "repo root") {
					t.Fatalf("want error naming both paths, got %v", err)
				}
				return
			}
			if err != nil || got != filepath.Join(dir, c.want) {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}
