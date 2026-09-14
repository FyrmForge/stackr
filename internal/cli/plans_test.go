package cli

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The plan verbs are the CI surface, a wrong method or path here means a
// pipeline silently reads plans it thinks it approved.
func TestPlanRoutes(t *testing.T) {
	cases := []struct {
		name         string
		call         func(c *Client) error
		method, path string
	}{
		{"list", func(c *Client) error {
			_, err := c.Plans(context.Background(), "p1", 5)
			return err
		}, http.MethodGet, "/api/v1/stacks/p1/config/plans"},
		{"get", func(c *Client) error {
			_, err := c.Plan(context.Background(), "pl1")
			return err
		}, http.MethodGet, "/api/v1/config/plans/pl1"},
		{"plan now", func(c *Client) error {
			_, err := c.PlanNow(context.Background(), "p1")
			return err
		}, http.MethodPost, "/api/v1/stacks/p1/config/plan"},
		{"approve", func(c *Client) error {
			_, err := c.ApprovePlan(context.Background(), "pl1")
			return err
		}, http.MethodPost, "/api/v1/config/plans/pl1/approve"},
		{"reject", func(c *Client) error {
			_, err := c.RejectPlan(context.Background(), "pl1")
			return err
		}, http.MethodPost, "/api/v1/config/plans/pl1/reject"},
		{"preview", func(c *Client) error {
			_, err := c.PlanPreview(context.Background(), "p1", "version: 1", map[string]string{"inc.yml": "x: 1"}, "prod")
			return err
		}, http.MethodPost, "/api/v1/stacks/p1/config/plan-preview"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotLimit string
			c, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				gotLimit = r.URL.Query().Get("limit")
				if tc.name == "list" || tc.name == "plan now" {
					_, _ = w.Write([]byte(`[{"id":"pl1","status":"pending","summary":"1 change"}]`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"pl1","status":"pending","summary":"1 change",` +
					`"changes":[{"kind":"delete","env":"prod","tile":"api"}],"destructive":true}`))
			})
			defer srv.Close()

			require.NoError(t, tc.call(c), tc.name)
			assert.Equal(t, tc.method, gotMethod)
			assert.Equal(t, tc.path, gotPath)
			if tc.name == "list" {
				assert.Equal(t, "5", gotLimit, "limit = %q, want 5", gotLimit)
			}
		})
	}
}

// A plan's changes and destructive flag are what a gate keys on; they must
// survive the decode.
func TestPlanDecodesChanges(t *testing.T) {
	c, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"pl1","status":"pending","summary":"2 changes","destructive":true,
			"changes":[{"kind":"delete","env":"prod","tile":"api"},
			           {"kind":"update","env":"prod","tile":"web","field":"image","old":"a","new":"b"}]}`))
	})
	defer srv.Close()

	p, err := c.Plan(context.Background(), "pl1")
	require.NoError(t, err)
	require.True(t, p.Destructive, "plan = %+v", p)
	require.Len(t, p.Changes, 2, "plan = %+v", p)
	assert.Equal(t, "image", p.Changes[1].Field, "update change = %+v", p.Changes[1])
	assert.Equal(t, "b", p.Changes[1].New, "update change = %+v", p.Changes[1])
}
