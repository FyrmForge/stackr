package panel

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
)

func calls(f *dockerfake.Fake) string {
	var b []string
	for _, c := range f.Calls() {
		if c.Method != "List" && c.Method != "Inspect" {
			b = append(b, c.Method+" "+strings.Join(c.Args, " "))
		}
	}
	return strings.Join(b, "; ")
}

func TestSwapKeepsOldUntilNewPasses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		health string
		want   string
	}{
		{"healthy", "Stop old; Run stackr-0.2.0 img:0.2.0; StopRemove old"},
		{"unhealthy", "Stop old; Run stackr-0.2.0 img:0.2.0; StopRemove new; Start old"},
	} {
		fake := dockerfake.New()
		fake.RunID = "new"
		fake.Containers = []docker.Container{
			{ID: "old", State: "running", Labels: map[string]string{LabelRole: "panel"}},
		}
		fake.Details = map[string]docker.Detail{"new": {Running: true, Health: tc.health}}
		l := New(fake)
		l.Poll, l.Deadline = time.Millisecond, 20*time.Millisecond
		err := l.Swap(ctx, docker.ContainerSpec{Name: "stackr-0.2.0", Image: "img:0.2.0"})
		if (err != nil) != (tc.health != "healthy") {
			t.Errorf("%s: err = %v", tc.health, err)
		}
		if got := calls(fake); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.health, got, tc.want)
		}
		if fake.Specs[0].Labels[LabelRole] != "panel" {
			t.Error("new panel not labelled")
		}
	}
}

func TestLaunchGuardAndSpecRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := dockerfake.New()
	l := New(fake)
	spec := docker.ContainerSpec{Name: "stackr-0.2.0", Image: "img:0.2.0", Env: []string{"A=1"}}
	if err := l.Launch(ctx, spec); err != nil {
		t.Fatal(err)
	}
	h := fake.Specs[0]
	if h.Image != "img:0.2.0" || !slices.Equal(h.Cmd, []string{"upgrade-swap"}) {
		t.Errorf("helper = %+v", h)
	}
	t.Setenv(SpecEnv, strings.TrimPrefix(h.Env[0], SpecEnv+"="))
	if got, err := SpecFromEnv(); err != nil || got.Name != spec.Name || got.Env[0] != "A=1" {
		t.Errorf("spec back = %+v %v", got, err)
	}
	fake.Containers = []docker.Container{
		{ID: "h", State: "running", Labels: map[string]string{LabelRole: "upgrader"}},
	}
	if err := l.Launch(ctx, spec); err == nil {
		t.Error("second launch while the helper runs")
	}
}

// An image already on the box is not pulled again.
func TestPullSkipsPresent(t *testing.T) {
	fake := dockerfake.New()
	if err := New(fake).Pull(context.Background(), "img:0.2.0", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range fake.Calls() {
		if c.Method == "Pull" {
			t.Error("pulled an image already present")
		}
	}
}
