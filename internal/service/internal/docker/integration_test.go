//go:build integration

// Integration tests against the local Docker daemon: make test-integration.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
		Name:   name,
		Image:  testImage,
		Cmd:    []string{"sh", "-c", "echo hello; echo oops >&2; sleep 300"},
		Env:    []string{"FOO=bar"},
		Labels: map[string]string{"stkr.test": name},
		Networks: []NetAttach{
			{Name: nets[0], Aliases: []string{"web"}},
			{Name: nets[1]},
		},
		Restart:         "unless-stopped",
		CapAdd:          []string{"NET_ADMIN"},
		HealthCmd:       "true",
		HealthIntervalS: 1,
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
	id, err := d.Run(ctx, ContainerSpec{
		Name:        uniq("stkr-it-host"),
		Image:       testImage,
		Cmd:         []string{"sleep", "300"},
		HostNetwork: true,
	})
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

func TestNetworksAndVolumes(t *testing.T) {
	d, ctx := newClient(t)
	net := uniq("stkr-it-net")
	labels := map[string]string{"stkr.test": net}
	for range 2 {
		if err := d.EnsureNetwork(ctx, net, labels); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = d.RemoveNetwork(context.Background(), net) })
	if names, err := d.ListNetworks(ctx, labels); err != nil || len(names) != 1 || names[0] != net {
		t.Fatalf("list networks: %v %v", names, err)
	}

	id, err := d.Run(ctx, ContainerSpec{Name: uniq("stkr-it"), Image: testImage, Cmd: []string{"sleep", "300"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.StopRemove(context.Background(), id) })
	for range 2 {
		if err := d.Connect(ctx, net, id, []string{"svc"}); err != nil {
			t.Fatal(err)
		}
	}
	if ip, cidr, err := d.MemberAddr(ctx, net, id); err != nil || ip == "" || cidr == "" {
		t.Fatalf("member addr: %q %q %v", ip, cidr, err)
	}
	if ids, err := d.NetworkMembers(ctx, net); err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("members: %v %v", ids, err)
	}
	if out, err := d.Exec(ctx, id, []string{"nslookup", "svc"}); err != nil {
		t.Fatalf("alias does not resolve: %q %v", out, err)
	}
	for range 2 {
		if err := d.Disconnect(ctx, net, id); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := d.RemoveNetwork(ctx, net); err != nil {
			t.Fatal(err)
		}
	}

	vol := uniq("stkr-it-vol")
	for range 2 {
		if err := d.CreateVolume(ctx, vol, "", nil, labels); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = d.RemoveVolume(context.Background(), vol) })
	vols, err := d.ListVolumes(ctx, labels)
	if err != nil || len(vols) != 1 || vols[0].Name != vol {
		t.Fatalf("list volumes: %+v %v", vols, err)
	}

	// Seed a file and a dotfile, tar it out, wipe-and-untar a changed volume
	// back, and check the originals are back and the stray is gone.
	if _, wait, err := d.volumeTool(ctx, vol, false,
		[]string{"sh", "-c", "echo a > /data/a; echo b > /data/.b"}, nil); err != nil {
		t.Fatal(err)
	} else if err := wait(); err != nil {
		t.Fatal(err)
	}
	var archive strings.Builder
	if err := d.TarVolume(ctx, vol, &archive, false); err != nil {
		t.Fatal(err)
	}
	if _, wait, err := d.volumeTool(ctx, vol, false, []string{"sh", "-c", "echo x > /data/stray"}, nil); err != nil {
		t.Fatal(err)
	} else if err := wait(); err != nil {
		t.Fatal(err)
	}
	if err := d.UntarVolume(ctx, vol, strings.NewReader(archive.String())); err != nil {
		t.Fatal(err)
	}
	out, wait, err := d.volumeTool(ctx, vol, true, []string{"sh", "-c", "cat /data/a /data/.b; ls /data"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(out)
	if err := wait(); err != nil || string(b) != "a\nb\na\n" {
		t.Fatalf("after untar: %q %v", b, err)
	}
	if _, wait, err := d.volumeTool(ctx, vol, true, []string{"sh", "-c", "exit 4"}, nil); err != nil {
		t.Fatal(err)
	} else if e, ok := IsExit(wait()); !ok || e.Code != 4 {
		t.Fatalf("tool exit: %v", e)
	}
	if info, err := d.InspectVolume(ctx, vol); err != nil || info.Name != vol ||
		len(info.UsedBy)+len(info.HeldBy) != 0 {
		t.Fatalf("inspect volume: %+v %v", info, err)
	}
	for range 2 {
		if err := d.RemoveVolume(ctx, vol); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.InspectVolume(ctx, vol); !errors.Is(err, ErrNotFound) {
		t.Fatalf("inspect removed volume: %v", err)
	}
}

func TestImages(t *testing.T) {
	d, ctx := newClient(t)
	var log strings.Builder

	// Pull a small public image by digest.
	dg, err := d.LocalDigest(ctx, testImage)
	if err != nil || !strings.HasPrefix(dg, "sha256:") {
		t.Fatalf("local digest: %q %v", dg, err)
	}
	if err := d.Pull(ctx, "alpine@"+dg, "", &log); err != nil {
		t.Fatalf("pull by digest: %v\n%s", err, log.String())
	}
	if err := d.Pull(ctx, "stackr-no-such-image-xyz:1", "", io.Discard); err == nil {
		t.Fatal("pull of a missing image succeeded")
	}

	// Build a hello Dockerfile.
	if exec.Command("docker", "buildx", "version").Run() != nil {
		t.Skip("no buildx")
	}
	dir := t.TempDir()
	df := "FROM " + testImage + "\nARG MSG\nRUN echo \"$MSG\" > /hello\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o600); err != nil {
		t.Fatal(err)
	}
	tag := uniq("stkr-it-img") + ":1"
	labels := map[string]string{"stkr.test": tag}
	id, err := d.Build(ctx, "default", dir, "Dockerfile", tag, map[string]string{"MSG": "hi"}, labels, &log)
	if err != nil || !strings.HasPrefix(id, "sha256:") {
		t.Fatalf("build: %q %v\n%s", id, err, log.String())
	}
	t.Cleanup(func() { _ = d.RemoveImage(context.Background(), id) })
	ims, err := d.ListImages(ctx, labels)
	if err != nil || len(ims) != 1 || ims[0].ID != id {
		t.Fatalf("list images: %+v %v", ims, err)
	}
	second := strings.TrimSuffix(tag, ":1") + ":2"
	if err := d.Tag(ctx, tag, second); err != nil {
		t.Fatal(err)
	}
	if removed, err := d.PruneImages(ctx, labels, []string{second}); err != nil || len(removed) != 0 {
		t.Fatalf("prune kept by tag: %v %v", removed, err)
	}
	removed, err := d.PruneImages(ctx, labels, nil)
	if err != nil || len(removed) != 2 {
		t.Fatalf("prune: %v %v", removed, err)
	}
	if ims, _ := d.ListImages(ctx, labels); len(ims) != 0 {
		t.Fatalf("still there: %+v", ims)
	}
}
