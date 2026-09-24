//go:build integration

package proxy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Against a throwaway Caddy container with its admin API bound off loopback.
func TestCaddy(t *testing.T) {
	admin := `{"admin":{"listen":"0.0.0.0:2019"}}`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "caddy.json"), []byte(admin), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1::2019",
		"-v", dir+":/cfg:ro", "caddy:2", "caddy", "run", "--config", "/cfg/caddy.json").Output()
	if err != nil {
		t.Fatalf("docker run caddy: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	port, err := exec.Command("docker", "port", id, "2019/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	c := New("http://" + strings.TrimSpace(strings.Split(string(port), "\n")[0]))
	ctx := context.Background()
	for i := 0; ; i++ {
		if _, err = c.Get(ctx); err == nil {
			break
		}
		if i == 60 {
			t.Fatalf("caddy admin never came up: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// The pushed config must keep the admin listener, or the next push has
	// nowhere to go.
	cfg := `{"admin":{"listen":"0.0.0.0:2019"},"apps":{"http":{"servers":{"s":{"listen":[":8080"],` +
		`"routes":[{"match":[{"host":["a.test"]}],"handle":[{"handler":"static_response","body":"hi"}]}]}}}}}`
	if err := c.Push(ctx, json.RawMessage(cfg)); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx)
	if err != nil || !strings.Contains(string(got), `"a.test"`) {
		t.Fatalf("read back: %s %v", got, err)
	}
	if err := c.Push(ctx, json.RawMessage(`{"admin":{"listen":"0.0.0.0:2019"},"apps":{"http":{"servers":{"s":{"listen":"not-a-list"}}}}}`)); err == nil {
		t.Fatal("invalid config accepted")
	}
	if got2, _ := c.Get(ctx); string(got2) != string(got) {
		t.Fatalf("rejected push changed the live config:\n%s\n%s", got, got2)
	}
}
