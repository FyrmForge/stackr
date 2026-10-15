//go:build integration

package registry

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// TestLoadIntoDocker loads a registry image into the local daemon and checks
// the id is one Image promised.
func TestLoadIntoDocker(t *testing.T) {
	srv := httptest.NewServer(ggcrregistry.New())
	defer srv.Close()
	l := (&Leaf{}).WithBase(srv.URL)
	img, err := random.Image(512, 2)
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	cf.OS, cf.Architecture = "linux", "amd64"
	cf.Config.Cmd = []string{"/x"}
	img, _ = mutate.ConfigFile(img, cf)
	ref, _ := name.NewTag(strings.TrimPrefix(srv.URL, "http://")+"/acme/app:1", name.Insecure)
	if err := remote.Write(ref, img, remote.WithContext(context.Background())); err != nil {
		t.Fatal(err)
	}
	idx := mutate.AppendManifests(mutate.IndexMediaType(empty.Index, "application/vnd.oci.image.index.v1+json"), mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}})
	iref, _ := name.NewTag(strings.TrimPrefix(srv.URL, "http://")+"/acme/app:multi", name.Insecure)
	if err := remote.WriteIndex(iref, idx); err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{"1", "multi"} {
		ci, err := l.Image(context.Background(), "acme/app", at, "amd64")
		if err != nil {
			t.Fatal(err)
		}
		f, _ := os.CreateTemp(t.TempDir(), "*.tar")
		tag := "localhost:5000/acme/app:loadtest-" + at
		if err := ci.Write(f, tag); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if out, err := exec.Command("docker", "load", "-i", f.Name()).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		out, err := exec.Command("docker", "image", "inspect", "-f", "{{.ID}}", tag).Output()
		if err != nil {
			t.Fatal(err)
		}
		id := strings.TrimSpace(string(out))
		t.Logf("%s: id %s, ids %v, digest %s", at, id, ci.IDs, ci.Digest)
		_ = exec.Command("docker", "image", "rm", tag).Run()
		if id != ci.IDs[0] && id != ci.IDs[1] {
			t.Fatalf("id %s not in %v", id, ci.IDs)
		}
	}
}
