package tile

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

func TestStateWord(t *testing.T) {
	c := func(st string) docker.Container { return docker.Container{State: st} }
	for name, tc := range map[string]struct {
		cs   []docker.Container
		want string
	}{
		"none":            {nil, "none"},
		"running":         {[]docker.Container{c("running")}, "running"},
		"stopped":         {[]docker.Container{c("exited")}, "stopped"},
		"paused":          {[]docker.Container{c("paused")}, "paused"},
		"all paused":      {[]docker.Container{c("paused"), c("paused")}, "paused"},
		"paused, exited":  {[]docker.Container{c("paused"), c("exited")}, "paused"},
		"one running":     {[]docker.Container{c("paused"), c("running")}, "degraded"},
		"some running":    {[]docker.Container{c("exited"), c("running")}, "degraded"},
		"unhealthy first": {[]docker.Container{{State: "paused", Health: "unhealthy"}}, "unhealthy"},
	} {
		if got := stateWord(tc.cs); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
