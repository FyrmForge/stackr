package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
)

func TestDecide(t *testing.T) {
	for _, c := range []struct {
		cur, want string
		mv        move
	}{
		{"0.8.1", "0.8.1", stay},
		{"0.8.2-dev.1", "0.8.2-dev.1", stay},
		{"0.8.1", "0.8.2", up},
		{"0.8.1", "0.10.0", up},
		{"0.8.1", "0.8.2-dev.1", up},       // dev over a release
		{"0.8.2-dev.1", "0.8.2-dev.2", up}, // dev over dev
		{"0.8.2-dev.1", "0.8.2", up},       // the release over its dev builds
		{"", "0.8.1", up},                  // a panel image that is not a release
		{"0.8.2-dev.1", "0.8.1", down},
		{"0.9.0", "0.8.5", down},
		{"0.8.2", "0.8.2-dev.3", down},
	} {
		if got := decide(c.cur, c.want); got != c.mv {
			t.Errorf("decide(%q, %q) = %d, want %d", c.cur, c.want, got, c.mv)
		}
	}
}

// The order is the contract: archive before anything changes, the panel
// after the proxy and registry, the workers only after the health check.
func TestUpgradeStepOrder(t *testing.T) {
	fakeDocker(t, "stackr-proxy", false)
	in := installspec.Input{Root: "x.io", PanelHost: "p.x.io", HTTPPort: "80", DataDir: "/data", Bind: "172.17.0.1"}
	var out bytes.Buffer
	steps := (runner{dry: true, out: &out}).upgradeSteps(in, "old", "0.8.2", true)
	var names []string
	for _, s := range steps {
		names = append(names, s.name)
	}
	want := []string{"pre-upgrade archive", "proxy", "registry", "panel swap", "health", "workers"}
	if !slices.Equal(names, want) {
		t.Fatalf("steps %v, want %v", names, want)
	}
	if err := runSteps(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	// What each step runs, in the order it must run.
	at := -1
	for _, mark := range []string{"panel-backup", installspec.ProxyName, installspec.RegistryName, "upgrade-swap", "upgrade-workers"} {
		i := strings.Index(out.String(), mark)
		if i <= at {
			t.Fatalf("%s out of order in:\n%s", mark, out.String())
		}
		at = i
	}
}

func TestRunStepsStops(t *testing.T) {
	var ran []string
	mk := func(name string, fail bool) step {
		return step{name, func(context.Context) error {
			ran = append(ran, name)
			if fail {
				return errors.New("boom")
			}
			return nil
		}}
	}
	err := runSteps(context.Background(), []step{mk("archive", false), mk("swap", true), mk("workers", false)})
	if err == nil || err.Error() != "swap: boom" || !slices.Equal(ran, []string{"archive", "swap"}) {
		t.Fatalf("err %v, ran %v", err, ran)
	}
}
