//go:build integration

// Integration tests against the local Docker daemon: make test-integration.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
)

const testImage = "alpine:3"

func newClient(t *testing.T) (*Client, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	d, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.cli.Ping(ctx); err != nil {
		t.Skipf("no docker daemon: %v", err)
	}
	if _, err := d.cli.ImageInspect(ctx, testImage); err != nil {
		rc, err := d.cli.ImagePull(ctx, testImage, image.PullOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	return d, ctx
}

func uniq(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }

func TestContainerLifecycle(t *testing.T) {
	d, ctx := newClient(t)
	nets := []string{uniq("stkr-it-a"), uniq("stkr-it-b")}
	for _, n := range nets {
		if _, err := d.cli.NetworkCreate(ctx, n, network.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.cli.NetworkRemove(context.Background(), n) })
	}
	name := uniq("stkr-it")
	id, err := d.Run(ctx, ContainerSpec{
		Name: name, Image: testImage,
		Cmd:       []string{"sh", "-c", "echo hello; echo oops >&2; sleep 300"},
		Env:       []string{"FOO=bar"},
		Labels:    map[string]string{"stkr.test": name},
		Networks:  []NetAttach{{Name: nets[0], Aliases: []string{"web"}}, {Name: nets[1]}},
		Restart:   "unless-stopped",
		CapAdd:    []string{"NET_ADMIN"},
		HealthCmd: "true", HealthIntervalS: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.StopRemove(context.Background(), id) })

	det, err := d.Inspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !det.Running || det.RestartCount != 0 || det.Name != name {
		t.Fatalf("inspect: %+v", det)
	}
	for _, n := range nets {
		if det.Networks[n] == "" {
			t.Errorf("no IP on %s: %v", n, det.Networks)
		}
	}
	if _, onBridge := det.Networks["bridge"]; onBridge {
		t.Error("joined the default bridge too")
	}

	list, err := d.List(ctx, map[string]string{"stkr.test": name})
	if err != nil || len(list) != 1 || list[0].ID != id || len(list[0].IPs) != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}

	out, err := d.Exec(ctx, id, []string{"sh", "-c", "echo $FOO"})
	if err != nil || strings.TrimSpace(out) != "bar" {
		t.Fatalf("exec: %q %v", out, err)
	}
	_, err = d.Exec(ctx, id, []string{"sh", "-c", "echo nope >&2; exit 3"})
	if e, ok := IsExit(err); !ok || e.Code != 3 || e.Stderr != "nope" {
		t.Fatalf("exec exit: %v", err)
	}

	r, wait, err := d.ExecStream(ctx, id, []string{"cat"}, strings.NewReader("piped"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	if err := wait(); err != nil || string(b) != "piped" {
		t.Fatalf("exec stream: %q %v", b, err)
	}

	logs, err := d.Logs(ctx, id, 10)
	if err != nil || !strings.Contains(logs, "hello") || !strings.Contains(logs, "oops") {
		t.Fatalf("logs: %q %v", logs, err)
	}
	ch, stop, err := d.StreamLogs(ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case l := <-ch:
			if strings.HasSuffix(l, "hello") && strings.HasPrefix(l, "O ") {
				seen["O"] = true
			}
			if strings.HasSuffix(l, "oops") && strings.HasPrefix(l, "E ") {
				seen["E"] = true
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("stream logs: saw %v", seen)
		}
	}
	stop()
	for range ch { // drain: stop must close the channel
	}

	if err := d.Restart(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := d.StopRemove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Inspect(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inspect after remove: %v", err)
	}
}

func TestHostNetwork(t *testing.T) {
	d, ctx := newClient(t)
	id, err := d.Run(ctx, ContainerSpec{Name: uniq("stkr-it-host"), Image: testImage,
		Cmd: []string{"sleep", "300"}, HostNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.StopRemove(context.Background(), id) })
	det, err := d.Inspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := det.Networks["host"]; !ok {
		t.Fatalf("not on host network: %v", det.Networks)
	}
}
