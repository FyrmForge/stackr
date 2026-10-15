package service

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
)

// testRegistry is a registry over TCP with the leaf on it.
func testRegistry(t *testing.T) (*registry.Leaf, string) {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return registry.New(nil, "").WithBase(srv.URL), strings.TrimPrefix(srv.URL, "http://")
}

func amd64Image(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(256, 1)
	must(t, err)
	cf, err := img.ConfigFile()
	must(t, err)
	cf.OS, cf.Architecture = "linux", "amd64"
	img, err = mutate.ConfigFile(img, cf)
	must(t, err)
	return img
}

// The controller loads a node only images its deploy may run: another org's
// is refused before anything is read; the org's own and stackr's are served.
func TestClusterImageRefusesAnotherOrgs(t *testing.T) {
	regs, host := testRegistry(t)
	for _, repo := range []string{"acme/shop/api", "stackr.system/pause"} {
		ref, err := name.NewTag(host+"/"+repo+":1", name.Insecure)
		must(t, err)
		must(t, remote.Write(ref, amd64Image(t)))
	}
	o := &Orchestrator{regs: regs}
	ctx := context.Background()
	if _, err := o.clusterImage(docker.WithPullOrg(ctx, "other"), "acme/shop/api", "1", "amd64"); !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("another org's image: %v", err)
	}
	for _, c := range []struct{ org, repo string }{{"acme", "acme/shop/api"}, {"other", "stackr.system/pause"}} {
		img, err := o.clusterImage(docker.WithPullOrg(ctx, c.org), c.repo, "1", "amd64")
		if err != nil || len(img.IDs) != 2 {
			t.Errorf("%s for %s: %+v, %v", c.repo, c.org, img, err)
		}
	}
	if _, err := o.clusterImage(ctx, "acme/shop/api", "2", "amd64"); err == nil {
		t.Error("a tag the registry lacks was served")
	}
}

// saveFake saves a real image, as a node's Docker does for a build.
type saveFake struct {
	*dockerfake.Fake
	img v1.Image
}

func (f saveFake) Save(_ context.Context, ref string, w io.Writer) error {
	tag, err := name.NewTag(ref, name.Insecure)
	if err != nil {
		return err
	}
	return tarball.Write(tag, f.img, w)
}

// A worker's build lands in the registry: its Docker saves it, the
// controller writes it, and the registry then holds it under the build's tag.
func TestBuildPushLandsInRegistry(t *testing.T) {
	regs, _ := testRegistry(t)
	o := &Orchestrator{regs: regs, cfg: Config{DataDir: t.TempDir()}, docker: saveFake{dockerfake.New(), amd64Image(t)}}
	ctx := docker.WithNode(context.Background(), "w1")
	res, err := o.pushLocal(ctx, "localhost:5000/acme/shop/api:c1")
	must(t, err)
	got, err := regs.Resolve(ctx, "acme/shop/api", "c1")
	must(t, err)
	if got == "" || got != res.Digest || len(res.Platforms) != 1 || res.Platforms[0] != "amd64" {
		t.Fatalf("registry has %q, push said %+v", got, res)
	}
}
