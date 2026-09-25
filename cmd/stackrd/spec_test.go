package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service"
)

// The installer runs the panel from RunArgs, the upgrade from panelSpec.
// Reading the args back must give the upgrade's spec exactly, so a field
// or env var added on one path only fails here.
func TestPanelSpecBothWays(t *testing.T) {
	in := installspec.Input{
		Root:      "example.com",
		PanelHost: "stkr.example.com",
		HTTPS:     true,
		Email:     "a@example.com",
		DNS01:     true,
		HTTPPort:  "80",
		HTTPSPort: "443",
		DataDir:   "/var/lib/stackr",
		Bind:      "172.17.0.1",
	}
	image := installspec.Image("0.2.0")
	upgrade := panelSpec(in)(image)
	install := fromArgs(t, installspec.Panel(image, in).RunArgs())
	if !reflect.DeepEqual(install, upgrade) {
		t.Fatalf("install and upgrade disagree:\ninstall %+v\nupgrade %+v", install, upgrade)
	}
	if !upgrade.HostNetwork || len(upgrade.CapAdd) == 0 || upgrade.Labels["stackr.role"] != "panel" {
		t.Errorf("panel lost host net, NET_ADMIN or its role label: %+v", upgrade)
	}
}

// fromArgs reads `docker run` args back; an unknown flag fails the test.
func fromArgs(t *testing.T, a []string) service.ContainerSpec {
	t.Helper()
	s := service.ContainerSpec{Labels: map[string]string{}}
	if a[0] != "run" || a[1] != "-d" {
		t.Fatalf("not a detached run: %v", a)
	}
	i := 2
	for ; i < len(a) && strings.HasPrefix(a[i], "-"); i += 2 {
		v := a[i+1]
		switch a[i] {
		case "--name":
			s.Name = v
		case "--restart":
			s.Restart = v
		case "--network":
			s.HostNetwork = v == "host"
		case "--cap-add":
			s.CapAdd = append(s.CapAdd, v)
		case "--label":
			k, val, _ := strings.Cut(v, "=")
			s.Labels[k] = val
		case "-v":
			s.Volumes = append(s.Volumes, v)
		case "-e":
			s.Env = append(s.Env, v)
		default:
			t.Fatalf("flag %s has no field in the upgrade spec", a[i])
		}
	}
	s.Image = a[i]
	if rest := a[i+1:]; len(rest) > 0 {
		s.Cmd = rest
	}
	return s
}
