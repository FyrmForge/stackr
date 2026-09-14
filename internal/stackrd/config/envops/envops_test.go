package envops_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The wizard hands an org with no domain of its own a default under the
// server's hostname, so its stacks get names at all. That row is older than
// anything the config file declares later, and the sort was oldest-first, so
// the default would have gone on beating the domain the file asked for.
func TestVisibleDomainResourcesPrefersDeclared(t *testing.T) {
	all := []repo.DomainResource{
		{ID: "d1", Level: "org", OwnerID: "o1", Host: "acme.panel.example", Declared: false},
		{ID: "d2", Level: "org", OwnerID: "o1", Host: "acme.com", Declared: true},
		{ID: "d3", Level: "instance", Host: "server.example", Declared: false},
		{ID: "d4", Level: "stack", OwnerID: "s1", Host: "stack.example", Declared: false},
	}
	got := envops.VisibleDomainResources(all, "s1", "o1")
	var hosts []string
	for _, r := range got {
		hosts = append(hosts, r.Host)
	}
	require.Equal(t, []string{"stack.example", "acme.com", "acme.panel.example", "server.example"}, hosts,
		"nearest level first, then declared ahead of the panel default")
}
