package docker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

func TestHealthWord(t *testing.T) {
	for status, want := range map[string]string{
		"Up 2 minutes (healthy)":          "healthy",
		"Up 2 minutes (unhealthy)":        "unhealthy",
		"Up 5 seconds (health: starting)": "starting",
		"Up 5 seconds":                    "",
		"Exited (1) 3 seconds ago":        "",
	} {
		if got := healthWord(status); got != want {
			t.Errorf("healthWord(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestPortBindings(t *testing.T) {
	exposed, bind := portBindings(map[string]string{"8080": "80", "53": "53", "53/udp": "53/udp"}, false)
	for _, p := range []nat.Port{"80/tcp", "53/udp"} {
		if _, ok := exposed[p]; !ok {
			t.Errorf("port %s not exposed", p)
		}
	}
	if b := bind["80/tcp"]; len(b) != 1 || b[0].HostPort != "8080" {
		t.Errorf("80/tcp bound to %v", b)
	}
	if b := bind["53/udp"]; len(b) != 1 || b[0].HostPort != "53" {
		t.Errorf("53/udp bound to %v", b)
	}
	if b := bind["53/tcp"]; len(b) != 1 || b[0].HostPort != "53" {
		t.Errorf("53/tcp bound to %v", b)
	}
	if _, b := portBindings(map[string]string{"80": "80"}, true); b["80/tcp"][0].HostIP != "" {
		t.Error("anyIP must leave HostIP empty (both families)")
	}
	if _, b := portBindings(map[string]string{"80": "80"}, false); b["80/tcp"][0].HostIP != "0.0.0.0" {
		t.Error("default stays IPv4")
	}
	if e, b := portBindings(nil, false); e != nil || b != nil {
		t.Error("no ports should give nil sets")
	}
}

func TestIsTarChanged(t *testing.T) {
	for msg, want := range map[string]bool{
		"exit status 1: tar: ./db: file changed as we read it": true,
		"exit status 1: tar: short read":                       true,
		"exit status 2: tar: permission denied":                false,
	} {
		if got := isTarChanged(errors.New(msg)); got != want {
			t.Errorf("isTarChanged(%q) = %v", msg, got)
		}
	}
}

func TestDrainProgress(t *testing.T) {
	stream := `{"status":"Pulling from library/alpine","id":"3"}
{"status":"Downloading","progress":"[==>   ]","id":"abc"}
{"status":"Digest: sha256:00"}
{"errorDetail":{"message":"denied"},"error":"denied"}
{"status":"after the error"}
`
	var log strings.Builder
	err := drainProgress(strings.NewReader(stream), &log)
	if err == nil || err.Error() != "denied" {
		t.Fatalf("stream error lost: %v", err)
	}
	if want := "3: Pulling from library/alpine\nDigest: sha256:00\nafter the error\n"; log.String() != want {
		t.Fatalf("log = %q", log.String())
	}
}

func TestPickDigest(t *testing.T) {
	rds := []string{"mirror/alpine@sha256:bbb", "alpine@sha256:aaa"}
	for ref, want := range map[string]string{
		"alpine:3":                   "sha256:aaa",
		"alpine":                     "sha256:aaa",
		"localhost:5000/other:1":     "sha256:bbb", // fallback: any digest
		"docker.io/library/alpine:3": "sha256:bbb", // no exact repo, first digest
	} {
		if got := pickDigest(ref, rds); got != want {
			t.Errorf("pickDigest(%q) = %q, want %q", ref, got, want)
		}
	}
	if pickDigest("x:1", nil) != "" {
		t.Error("built locally should be empty")
	}
	// A registry port is not a tag.
	if got := pickDigest("reg:5000/app", []string{"x@sha256:1", "reg:5000/app@sha256:2"}); got != "sha256:2" {
		t.Errorf("port read as tag: %q", got)
	}
}

// An import is pulled as localhost:5000/<org>/... and tagged with the upstream
// name, so its RepoDigests only name the registry repo; the digest must still
// be the upstream index digest, never "" (that would read as an update).
func TestPickDigestRetaggedImport(t *testing.T) {
	rds := []string{"localhost:5000/acme/docker.io/library/postgres@sha256:index"}
	for _, ref := range []string{"postgres:16", "postgres", "docker.io/library/postgres:16", "ghcr.io/foo/bar:1"} {
		if got := pickDigest(ref, rds); got != "sha256:index" {
			t.Errorf("pickDigest(%q) = %q, want the index digest", ref, got)
		}
	}
}

func TestParsePrune(t *testing.T) {
	n, total, _ := parsePrune("abc\ndef\nTotal:\t1.2GB\n")
	if n != 2 || total != "1.2GB" {
		t.Errorf("got %d %q", n, total)
	}
	if n, total, _ = parsePrune("Total:\t0B\n"); n != 0 || total != "0B" {
		t.Errorf("empty: %d %q", n, total)
	}
}

func TestLogConfigCapsJSONFile(t *testing.T) {
	if logConfig.Type != "json-file" || logConfig.Config["max-size"] != "10m" || logConfig.Config["max-file"] != "3" {
		t.Fatalf("logConfig = %+v, want json-file 3 x 10m", logConfig)
	}
}

func TestMounts(t *testing.T) {
	got := mounts([]Mount{
		{Volume: "files-a", Subpath: "0/app.conf", Target: "/etc/app.conf", ReadOnly: true},
		{Volume: "data", Target: "/data"},
	})
	if len(got) != 2 {
		t.Fatalf("mounts = %+v", got)
	}
	if m := got[0]; m.Type != "volume" || m.Source != "files-a" || m.Target != "/etc/app.conf" || !m.ReadOnly ||
		m.VolumeOptions == nil || m.VolumeOptions.Subpath != "0/app.conf" {
		t.Errorf("subpath mount = %+v", m)
	}
	if m := got[1]; m.ReadOnly || m.VolumeOptions != nil {
		t.Errorf("plain mount = %+v, want read-write with no subpath", m)
	}
	if len(mounts(nil)) != 0 {
		t.Error("no mounts must map to none")
	}
}

func TestMountDenied(t *testing.T) {
	spec := ContainerSpec{Volumes: []string{"stackr-share-a1-abc:/data:ro", "/host:/h"}, Mounts: []Mount{{Volume: "stackr-vol-x"}}}
	nfs := errors.New(`Error response from daemon: failed to mount local volume: mount :/srv:/var/lib/docker/volumes/stackr-share-a1-abc/_data: permission denied`)
	var md MountDeniedError
	if err := mountDenied(nfs, specVolumes(spec)); !errors.As(err, &md) || md.Volume != "stackr-share-a1-abc" || md.Raw != nfs.Error() {
		t.Errorf("nfs = %v", err)
	}
	other := errors.New("no such image")
	if err := mountDenied(other, specVolumes(spec)); err != other {
		t.Errorf("unrelated wrapped: %v", err)
	}
	if err := mountDenied(errors.New("permission denied on /etc/foo"), specVolumes(spec)); errors.As(err, &md) && md.Volume != "stackr-share-a1-abc" {
		t.Errorf("unnamed volume wrapped: %v", err)
	}
}

// The daemon names the share volume with its path and "/_data" on the end;
// Start must still find the volume name in it.
func TestStartMountDeniedFullWording(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"error while mounting volume '/var/lib/docker/volumes/stackr-share-x/_data': failed to mount local volume: mount :/export:/var/lib/docker/volumes/stackr-share-x/_data: permission denied"}`))
	}))
	defer srv.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	err = (&Client{cli: cli}).Start(context.Background(), "c1")
	var md MountDeniedError
	if !errors.As(err, &md) || md.Volume != "stackr-share-x" {
		t.Fatalf("got %#v", err)
	}
}

// Credentials the daemon echoes on a failed start never reach the caller.
func TestStartMasksCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"mount //nas/x: addr=1.2.3.4,username=bob,password=hunter2: i/o error"}`))
	}))
	defer srv.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	err = (&Client{cli: cli}).Start(context.Background(), "c1")
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "bob") {
		t.Fatalf("not masked: %v", err)
	}
}

// Restart and Create mask echoed credentials too, and keep the cause.
func TestRestartCreateMaskCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"mount //nas/x: addr=1.2.3.4,username=bob,password=hunter2: i/o error"}`))
	}))
	defer srv.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{cli: cli}
	_, cerr := c.Create(context.Background(), ContainerSpec{Name: "x", Image: "i"})
	for name, err := range map[string]error{"restart": c.Restart(context.Background(), "c1"), "create": cerr} {
		if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "bob") {
			t.Fatalf("%s not masked: %v", name, err)
		}
		var me maskedErr
		if !errors.As(err, &me) {
			t.Fatalf("%s lost the wrapper: %v", name, err)
		}
	}
}
