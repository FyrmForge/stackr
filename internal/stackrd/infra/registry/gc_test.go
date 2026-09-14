package registry_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The client owns the syntax rail: every call below builds a URL out of the
// name and the tag, so a traversal has to die here rather than in whichever
// handler happened to remember. No network is reached: a refused reference
// never gets as far as a request.
func TestClientRefusesReferencesThatAreNotNames(t *testing.T) {
	cl := registry.NewClient(&repo.Registry{URL: "127.0.0.1:5000", Username: "stackr"}, nil)
	ctx := context.Background()

	bad := []string{
		"acme_shop_api/../../other_image", // the escaped form echo hands back decoded
		"../other_image",
		"acme_shop_api/..",
		"acme_shop_api/nested", // flat names only, so a slash is never legitimate
		"",
		"Acme_Shop", // the catalog is lowercase
	}
	for _, name := range bad {
		_, err := cl.Tags(ctx, name)
		assert.ErrorContains(t, err, "not a repository name", "Tags(%q)", name)
		_, err = cl.Tag(ctx, name, "v1")
		assert.ErrorContains(t, err, "not a repository name", "Tag(%q)", name)
		assert.ErrorContains(t, cl.DeleteTag(ctx, name, "v1"), "not a repository name",
			"DeleteTag(%q)", name)
	}

	for _, tag := range []string{"../../v1", "v1/x", "-leading", ""} {
		_, err := cl.Tag(ctx, "acme_shop_api", tag)
		assert.ErrorContains(t, err, "not a tag", "Tag(tag=%q)", tag)
	}

	// A real name gets past the rail and stops at the nil signer, which is
	// what says the registry keypair never loaded.
	_, err := cl.Tags(ctx, "acme_shop_api")
	require.ErrorContains(t, err, "token signing is not configured")
}

// The panel is a swarm service, so localhost inside it is the panel, not the
// registry: the registry publishes its port in host mode, which is exact on
// the manager's host and unreachable from any container. Every catalog and
// manifest read the panel makes has to go over the overlay by service name.
func TestPanelAddrIsTheOverlayServiceNotLocalhost(t *testing.T) {
	assert.Equal(t, "stkr-registry:5000", registry.PanelAddr(&repo.Registry{URL: "localhost:5000"}))
	assert.Equal(t, "stkr-registry:5001", registry.PanelAddr(&repo.Registry{URL: "localhost:5001"}))
	// A TLS domain is for nodes pulling, never for this hop.
	assert.Equal(t, "stkr-registry:5000",
		registry.PanelAddr(&repo.Registry{URL: "localhost:5000", Domain: "reg.example.com"}))
	assert.Equal(t, "stkr-registry:5000", registry.PanelAddr(&repo.Registry{URL: "localhost"}))
}
