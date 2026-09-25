package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUnder(t *testing.T) {
	r := Repo{Dir: "/data/repos/t1"}
	for _, p := range []string{"../../keys", "..", "sub/../../other", "../tile2"} {
		if _, err := r.Under(p); err == nil {
			t.Errorf("Under(%q) allowed", p)
		}
	}
	for _, p := range []string{".", "", "app", "..hidden", "/abs/is/rooted/in/repo", "a/../b"} {
		if _, err := r.Under(p); err != nil {
			t.Errorf("Under(%q) refused: %v", p, err)
		}
	}
}

func TestImageName(t *testing.T) {
	if got := ImageName("stkr/acme_shop_api", "9f3c1ab22", "job-1234567890"); got != "stkr/acme_shop_api:9f3c1ab" {
		t.Error(got)
	}
	if got := ImageName("r", "", "abcdef123456"); got != "r:abcdef12" {
		t.Error(got)
	}
	if got := ImageName("r", "", "ab"); got != "r:ab" {
		t.Error(got)
	}
}

// fixture makes a bare repo with two commits on main and returns its path and
// both SHAs. The dev box's global git config is kept out.
func fixture(t *testing.T) (bare, first, second string) {
	t.Helper()
	allowProtocol = "https:ssh:file"
	t.Cleanup(func() { allowProtocol = "https:ssh" })
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	bare, work := filepath.Join(root, "bare.git"), filepath.Join(root, "work")
	g := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(
			"git",
			append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...,
		)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	g(root, "init", "-q", "--bare", "-b", "main", bare)
	g(root, "init", "-q", "-b", "main", work)
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("stackr-compose.yml", "v1\n")
	write("app/main.go", "package main\n")
	g(work, "add", ".")
	g(work, "commit", "-q", "-m", "one")
	first = g(work, "rev-parse", "HEAD")
	write("stackr-compose.yml", "v2\n")
	write("docs/readme", "hi\n")
	g(work, "add", ".")
	g(work, "commit", "-q", "-m", "two")
	second = g(work, "rev-parse", "HEAD")
	g(work, "push", "-q", bare, "main")
	return bare, first, second
}

func TestCheckoutReadDiff(t *testing.T) {
	bare, first, second := fixture(t)
	ctx := context.Background()
	var authCalled bool
	r := Repo{
		Dir:    filepath.Join(t.TempDir(), "clone"),
		URL:    bare,
		Branch: "main",
		Auth: func(context.Context) []string {
			authCalled = true
			return []string{"GIT_CONFIG_COUNT=0"}
		},
	}
	var log strings.Builder

	sha, err := r.Checkout(ctx, "", &log)
	if err != nil || sha != second {
		t.Fatalf("checkout branch: %q %v\n%s", sha, err, log.String())
	}
	if !authCalled {
		t.Error("auth env not used")
	}
	if sha, err := r.Checkout(ctx, first, &log); err != nil || sha != first {
		t.Fatalf("checkout commit: %q %v\n%s", sha, err, log.String())
	}
	if b, _ := os.ReadFile(filepath.Join(r.Dir, "stackr-compose.yml")); string(b) != "v1\n" {
		t.Fatalf("tree not at first commit: %q", b)
	}
	if b, err := r.ReadFile(ctx, second, "stackr-compose.yml"); err != nil || string(b) != "v2\n" {
		t.Fatalf("read at commit: %q %v", b, err)
	}
	if _, err := r.ReadFile(ctx, second, "nope"); err == nil {
		t.Fatal("missing file read")
	}
	paths, err := r.ChangedPaths(ctx, first, second)
	if err != nil || !slices.Equal(paths, []string{"docs/readme", "stackr-compose.yml"}) {
		t.Fatalf("changed paths: %v %v", paths, err)
	}
	if _, err := r.ChangedPaths(ctx, "--output=/tmp/x", second); err == nil {
		t.Fatal("flag-looking ref accepted")
	}
	if br := r.Branches(ctx); !slices.Equal(br, []string{"main"}) {
		t.Fatalf("branches: %v", br)
	}

	// A half-written clone (no .git) is thrown away and redone.
	broken := Repo{Dir: filepath.Join(t.TempDir(), "broken"), URL: bare}
	if err := os.MkdirAll(broken.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if sha, err := broken.Checkout(ctx, "", &log); err != nil || sha != second {
		t.Fatalf("redo clone: %q %v", sha, err)
	}
}

func TestProtocolAllowlist(t *testing.T) {
	bare, _, _ := fixture(t)
	allowProtocol = "https:ssh"
	r := Repo{Dir: filepath.Join(t.TempDir(), "c"), URL: "file://" + bare}
	if _, err := r.Checkout(context.Background(), "", &strings.Builder{}); err == nil {
		t.Fatal("file:// clone allowed under https:ssh")
	}
}
