package docker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

// buildx prints a header before the ids; neither the header nor a stderr
// warning is a removed entry.
func TestParsePruneSkipsNoise(t *testing.T) {
	n, total, _ := parsePrune("Deleted build cache objects:\nabc\ndef\nTotal:\t1.2GB\n")
	if n != 2 || total != "1.2GB" {
		t.Errorf("n=%d total=%q", n, total)
	}
	if n, _, _ = parsePrune("WARNING: something odd happened\nabc\n"); n != 1 {
		t.Errorf("a warning line counted: n=%d", n)
	}
}

// A box that never built anything has no builder: nothing to prune, not a
// failure of the nightly job.
func TestBuildCacheResultNoBuilder(t *testing.T) {
	exit := errors.New("exit status 1")
	n, total, err := buildCacheResult("", "ERROR: no builder \"stackr-builder\" found\n", exit)
	if err != nil || n != 0 || total != "" {
		t.Errorf("n=%d total=%q err=%v", n, total, err)
	}
	if _, _, err = buildCacheResult("", "ERROR: daemon down\n", exit); err == nil {
		t.Error("a real failure was swallowed")
	}
}

// Only a cluster registry ref gets a minted pass, for its repository; any
// other registry keeps the caller's auth.
func TestClusterRepo(t *testing.T) {
	for ref, want := range map[string]string{
		"localhost:5000/acme/shop/api:v1":                      "acme/shop/api",
		"localhost:5000/stackr.system/stackr@sha256:abc":       "stackr.system/stackr",
		"localhost:5000/acme/docker.io/library/postgres":       "acme/docker.io/library/postgres",
		"ghcr.io/acme/api:v1":                                  "",
		"localhost:5001/acme/shop/api:v1":                      "",
		"registry.example.com/localhost:5000/acme/shop/api:v1": "",
	} {
		got, ok := clusterRepo(ref)
		if got != want || ok != (want != "") {
			t.Errorf("%s: %q %v, want %q", ref, got, ok, want)
		}
	}
}

// A cluster registry tag is pulled by the digest the registry answers, then
// tagged; no digest is an error and nothing is pulled. Other registries and
// digest refs go as given.
func TestPullClusterTagByDigest(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case strings.HasSuffix(r.URL.Path, "/images/create"):
			calls = append(calls, "pull "+q.Get("fromImage")+" "+q.Get("tag"))
		case strings.HasSuffix(r.URL.Path, "/tag"):
			calls = append(calls, "tag "+strings.TrimSuffix(r.URL.Path[strings.Index(r.URL.Path, "/images/")+8:], "/tag")+" "+q.Get("repo")+":"+q.Get("tag"))
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	dg := "sha256:" + strings.Repeat("a", 64)
	d := &Client{cli: cli}
	d.SetRegistryDigest(func(_ context.Context, repo, tag string) (string, error) {
		if repo == "acme/shop/api" && tag == "v1" {
			return dg, nil
		}
		return "", nil
	})
	ctx := context.Background()
	if err := d.Pull(ctx, "localhost:5000/acme/shop/api:v1", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := d.Pull(ctx, "localhost:5000/acme/shop/api:v2", "", nil); err == nil {
		t.Error("a tag with no digest was pulled")
	}
	for _, ref := range []string{"localhost:5000/acme/shop/api@" + dg, "ghcr.io/acme/api:v1"} {
		if err := d.Pull(ctx, ref, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"pull localhost:5000/acme/shop/api " + dg,
		"tag localhost:5000/acme/shop/api@" + dg + " localhost:5000/acme/shop/api:v1",
		"pull localhost:5000/acme/shop/api " + dg,
		"pull ghcr.io/acme/api v1",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}
