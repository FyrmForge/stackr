package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// The volume routes carry a server id, and until this guard existed they
// ignored it: every create and delete ran against the manager, so deleting a
// worker's volume destroyed the manager's volume of the same name instead.
//
// The empty node_id case is the sharp one. agent.Nodes.IsSelf reads an empty
// node ID as "this machine", so a server row that has not joined yet would
// collapse onto the manager rather than being refused, the same silent
// wrong-node write, arriving by a different route.
//
// The server detail page branches on the same emptiness, and for the same
// reason: it used to render the manager's docker version, core count and whole
// volume list as if they belonged to a machine with no docker on it.
// One condition, two callers.
func TestVolumeNodeResolvesTheServerAndRefusesOneThatHasNotJoined(t *testing.T) {
	store := testdb.New(t)
	h := &handler{store: store}
	ctx := context.Background()

	require.NoError(t, store.CreateServer(ctx, &repo.Server{
		ID: "joined", Name: "worker-2", NodeID: "ufup1iv9z8vnpj2yubolb9ga6", Status: "ready",
	}))
	require.NoError(t, store.CreateServer(ctx, &repo.Server{
		ID: "pending", Name: "worker-1", NodeID: "", Status: "pending",
	}))

	call := func(id string) (string, error) {
		e := echo.New()
		c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
		c.SetParamNames("id")
		c.SetParamValues(id)
		return h.volumeNode(c)
	}

	node, err := call("joined")
	require.NoError(t, err, "a joined node resolves")
	assert.Equal(t, "ufup1iv9z8vnpj2yubolb9ga6", node,
		"the volume routes must target the node in the row, not the manager")

	_, err = call("pending")
	require.Error(t, err, "a server with no node_id must be refused, not sent to the manager")
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusConflict, he.Code)

	_, err = call("no-such-server")
	require.Error(t, err, "an unknown server id must not fall through to the manager")
	require.ErrorAs(t, err, &he)
	assert.Equal(t, http.StatusNotFound, he.Code)
}
