package upgrade

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/panel"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.1.1", "v0.1.0", true},
		{"v0.2.0", "v0.1.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"dev", "v0.1.0", false},
		{"v0.1.0", "dev", false},
		{"0.2.0", "v0.1.0", false},
		{"", "v0.1.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
	if got := ImageRef("v0.1.1"); got != "ghcr.io/fyrmforge/stackr:0.1.1" {
		t.Errorf("ImageRef = %q", got)
	}
}

func TestCheckAndUpgrade(t *testing.T) {
	ctx := context.Background()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tag_name":"v0.2.0"}`)
	}))
	defer gh.Close()
	fake := dockerfake.New()
	fake.Err = map[string]error{"LocalDigest": errors.New("no such image")}
	var order []string
	f := &Flow{
		Panel:   panel.New(fake),
		Version: "v0.1.0",
		URL:     gh.URL,
		Archive: func(context.Context, io.Writer) (string, error) {
			order = append(order, "archive")
			return "/data/backups/pre-upgrade-v0.1.0.tar.gz", nil
		},
		Spec: func(image string) docker.ContainerSpec { return docker.ContainerSpec{Name: "stackr", Image: image} },
	}

	tag, err := f.Check(ctx)
	if err != nil || tag != "v0.2.0" {
		t.Fatalf("check = %q %v", tag, err)
	}
	if _, ok := f.Available(tag); !ok {
		t.Error("v0.2.0 not offered over v0.1.0")
	}
	if _, err := f.Upgrade(ctx, "v0.1.0", io.Discard); err == nil {
		t.Error("upgrade to the same version ran")
	}

	archive, err := f.Upgrade(ctx, tag, io.Discard)
	if err != nil || !strings.HasSuffix(archive, ".tar.gz") {
		t.Fatalf("upgrade = %q %v", archive, err)
	}
	var seq []string
	for _, c := range fake.Calls() {
		if c.Method == "Pull" || c.Method == "Run" {
			seq = append(seq, c.Method+" "+c.Args[0])
		}
	}
	if strings.Join(seq, ", ") != "Pull ghcr.io/fyrmforge/stackr:0.2.0, Run stackr-upgrader" || len(order) != 1 {
		t.Errorf("sequence = %v, archive calls %d", seq, len(order))
	}
	var sp docker.ContainerSpec
	t.Setenv(panel.SpecEnv, strings.TrimPrefix(fake.Specs[0].Env[0], panel.SpecEnv+"="))
	if sp, err = panel.SpecFromEnv(); err != nil || sp.Name != "stackr-0.2.0" ||
		sp.Env[0] != "STACKR_IMAGE=ghcr.io/fyrmforge/stackr:0.2.0" {
		t.Errorf("new panel spec = %+v %v", sp, err)
	}

	// A failed archive never reaches the helper.
	fake2 := dockerfake.New()
	f.Panel = panel.New(fake2)
	f.Archive = func(context.Context, io.Writer) (string, error) { return "", errors.New("disk full") }
	if _, err := f.Upgrade(ctx, tag, io.Discard); err == nil || len(fake2.Specs) != 0 {
		t.Errorf("archive failure: %v, runs %d", err, len(fake2.Specs))
	}

	// Dev builds refuse every path.
	f.Version = "dev"
	if tag, _ := f.Check(ctx); tag != "" {
		t.Error("dev build checked")
	}
	if _, err := f.Upgrade(ctx, "v9.0.0", io.Discard); err == nil {
		t.Error("dev build upgraded")
	}
}
