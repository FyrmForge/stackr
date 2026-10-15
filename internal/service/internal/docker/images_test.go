package docker

import (
	"context"
	"errors"
	"io"
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

func TestClusterRef(t *testing.T) {
	for ref, want := range map[string]string{
		"localhost:5000/acme/shop/api:v1":                      "acme/shop/api v1",
		"localhost:5000/stackr.system/stackr@sha256:abc":       "stackr.system/stackr sha256:abc",
		"localhost:5000/acme/docker.io/library/postgres":       "acme/docker.io/library/postgres latest",
		"ghcr.io/acme/api:v1":                                  "",
		"localhost:5001/acme/shop/api:v1":                      "",
		"registry.example.com/localhost:5000/acme/shop/api:v1": "",
	} {
		repo, at, ok := clusterRef(ref)
		if got := strings.TrimSpace(repo + " " + at); got != want || ok != (want != "") {
			t.Errorf("%s: %q %v, want %q", ref, got, ok, want)
		}
	}
	if got := localName("localhost:5000/a/b@sha256:ff"); got != "localhost:5000/a/b:sha256-ff" {
		t.Errorf("digest ref named %s here", got)
	}
}

// A cluster image is loaded, never pulled: kept when what is here has an id
// the registry's copy gives, refused when the source refuses, and a load
// whose id is not the registry's is removed and fails. Another registry is
// pulled as before.
func TestPullClusterLoads(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	var calls []string
	ids := map[string]string{} // name -> id here
	loads := good
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path[strings.Index(r.URL.Path[1:], "/")+1:] // drop /v1.NN
		switch {
		case p == "/info":
			_, _ = w.Write([]byte(`{"Architecture":"x86_64"}`))
		case strings.HasSuffix(p, "/json") && strings.HasPrefix(p, "/images/"):
			name := strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json")
			if id, ok := ids[name]; ok {
				_, _ = w.Write([]byte(`{"Id":"` + id + `"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no such image"}`))
		case p == "/images/load":
			b, _ := io.ReadAll(r.Body)
			name := string(b)
			calls = append(calls, "load "+name)
			ids[name] = loads
			_, _ = w.Write([]byte(`{"stream":"Loaded image"}`))
		case r.Method == http.MethodDelete:
			name := strings.TrimPrefix(p, "/images/")
			calls = append(calls, "rm "+name)
			delete(ids, name)
			_, _ = w.Write([]byte(`[]`))
		case p == "/images/create":
			calls = append(calls, "pull "+r.URL.Query().Get("fromImage"))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	d := &Client{cli: cli}
	d.SetClusterSource(func(_ context.Context, repo, at, arch string) (ClusterImage, error) {
		if repo != "acme/api" || arch != "amd64" {
			return ClusterImage{}, errors.New("not yours")
		}
		return ClusterImage{Digest: "sha256:idx", IDs: []string{good, "sha256:cfg"},
			Write: func(w io.Writer, tag string) error { _, err := io.WriteString(w, tag); return err }}, nil
	})
	ctx := context.Background()
	step := func(ref string, ok bool, want ...string) {
		t.Helper()
		calls = nil
		if err := d.Pull(ctx, ref, "", nil); (err == nil) != ok {
			t.Errorf("%s: %v", ref, err)
		}
		if !slices.Equal(calls, want) {
			t.Errorf("%s: calls %v, want %v", ref, calls, want)
		}
	}
	step("localhost:5000/acme/api:v1", true, "load localhost:5000/acme/api:v1")
	step("localhost:5000/acme/api:v1", true) // here already: no transfer
	if dg, err := d.LocalDigest(ctx, "localhost:5000/acme/api:v1"); err != nil || dg != "sha256:idx" {
		t.Errorf("LocalDigest = %q, %v", dg, err)
	}
	ids["localhost:5000/acme/api:v1"] = "sha256:planted" // a tag here proves nothing
	if _, err := d.LocalDigest(ctx, "localhost:5000/acme/api:v1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a planted image counted: %v", err)
	}
	step("localhost:5000/acme/api:v1", true, "load localhost:5000/acme/api:v1")
	step("localhost:5000/acme/api@"+good, true, "load localhost:5000/acme/api:sha256-"+strings.Repeat("a", 64))
	step("localhost:5000/other/api:v1", false) // refused: nothing moves
	loads = "sha256:evil"
	step("localhost:5000/acme/api:v2", false, "load localhost:5000/acme/api:v2", "rm localhost:5000/acme/api:v2")
	step("ghcr.io/acme/api:v1", true, "pull ghcr.io/acme/api")
}
