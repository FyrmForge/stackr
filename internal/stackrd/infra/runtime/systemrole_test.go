package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The registry was not protected infrastructure, so the containers
// page offered stop and remove on it. Every worker pulls tile images from it
// and the manager's builds push to it, so stopping it breaks the next deploy
// with a pull error that never mentions the registry.
//
// systemRoleLabel is the half that needs no daemon; the panel is identified by
// container id instead and is covered by the self lookup above it.
func TestSystemRoleProtectsInfrastructure(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"agent", map[string]string{LabelAgentTask: "true"}, "agent"},
		{"proxy", map[string]string{"stackr.traefik": "true"}, "proxy"},
		{"registry", map[string]string{"stackr.registry": "true"}, "registry"},
		{"a tile is ordinary", map[string]string{"stackr.app": "abc"}, ""},
		{"the label must say true", map[string]string{"stackr.registry": "false"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, systemRoleLabel(c.labels))
		})
	}
}
