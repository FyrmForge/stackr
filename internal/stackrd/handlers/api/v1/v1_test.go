package v1

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/stackconf"
)

// buildSpec constructs the reflector the same way the openapi generator does:
// Register only reflects types, so nil deps are fine.
func buildSpec(t *testing.T) specDoc {
	t.Helper()
	a := New(nil, nil, nil, nil, nil, nil, nil, nil, nil, stackconf.Applier{})
	a.Register(echo.New().Group("/api/v1"))
	raw, err := a.SpecJSON()
	require.NoError(t, err, "SpecJSON")
	var doc specDoc
	require.NoError(t, json.Unmarshal(raw, &doc), "unmarshal spec")
	return doc
}

type specDoc struct {
	Paths      map[string]map[string]specOp `json:"paths"`
	Components struct {
		SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
	} `json:"components"`
}

type specOp struct {
	OperationID string                     `json:"operationId"`
	Responses   map[string]json.RawMessage `json:"responses"`
	Parameters  []struct {
		Name string `json:"name"`
		In   string `json:"in"`
	} `json:"parameters"`
}

// op returns the operation for a method+path, failing if absent.
func (d specDoc) op(t *testing.T, method, path string) specOp {
	t.Helper()
	o, ok := d.Paths[path][method]
	require.True(t, ok, "no %s %s in spec", method, path)
	return o
}

func TestSpec_PathsAndSecurity(t *testing.T) {
	doc := buildSpec(t)
	assert.Len(t, doc.Paths, 98, "paths")
	assert.Contains(t, doc.Components.SecuritySchemes, securityName, "security scheme missing from spec")
}

func TestSpec_OperationIDsUnique(t *testing.T) {
	doc := buildSpec(t)
	seen := map[string]string{} // id -> "METHOD path"
	for path, methods := range doc.Paths {
		for method, o := range methods {
			if o.OperationID == "" {
				assert.Fail(t, "missing operationId", "%s %s has no operationId", method, path)
				continue
			}
			if prev, dup := seen[o.OperationID]; dup {
				assert.Fail(t, "duplicate operationId", "duplicate operationId %q on %s %s and %s", o.OperationID, method, path, prev)
			}
			seen[o.OperationID] = method + " " + path
		}
	}
}

func TestSpec_StatusCodesMatchHandlers(t *testing.T) {
	doc := buildSpec(t)
	cases := []struct {
		method, path, want string
	}{
		{"post", "/api/v1/stacks/{id}/apps", "201"}, // create
		{"delete", "/api/v1/apps/{id}", "204"},      // delete, no body
		{"post", "/api/v1/apps/{id}/deploy", "202"}, // accepted
		{"get", "/api/v1/apps/{id}", "200"},         // read
	}
	for _, c := range cases {
		o := doc.op(t, c.method, c.path)
		assert.Contains(t, o.Responses, c.want, "%s %s: no %s response (have %v)", c.method, c.path, c.want, keys(o.Responses))
	}
}

func TestSpec_ErrorResponsesPresent(t *testing.T) {
	doc := buildSpec(t)
	o := doc.op(t, "get", "/api/v1/apps/{id}")
	for _, code := range []string{"401", "403", "404"} {
		assert.Contains(t, o.Responses, code, "get /apps/{id}: missing %s response (have %v)", code, keys(o.Responses))
	}
}

func TestSpec_LogsHasTailAndFollow(t *testing.T) {
	doc := buildSpec(t)
	o := doc.op(t, "get", "/api/v1/apps/{id}/logs")
	want := map[string]bool{"tail": false, "follow": false}
	for _, p := range o.Parameters {
		if p.In == "query" {
			want[p.Name] = true
		}
	}
	for name, found := range want {
		assert.True(t, found, "logs endpoint missing query param %q", name)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestOperationID(t *testing.T) {
	cases := map[string]string{
		"Get app":               "getApp",
		"List app deployments":  "listAppDeployments",
		"Replace env variables": "replaceEnvVariables",
		"Create stack":          "createStack",
	}
	for in, want := range cases {
		assert.Equal(t, want, operationID(in), "operationID(%q)", in)
	}
}

func TestSuccessStatus(t *testing.T) {
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/apps/:id/deploy", http.StatusAccepted},
		{http.MethodPost, "/stacks", http.StatusCreated},
		{http.MethodDelete, "/apps/:id", http.StatusNoContent},
		{http.MethodGet, "/apps", http.StatusOK},
		{http.MethodPatch, "/apps/:id", http.StatusOK},
		{http.MethodPut, "/apps/:id/variables", http.StatusOK},
		// a computation, not a creation, despite being a POST
		{http.MethodPost, "/stacks/:id/config/plan-preview", http.StatusOK},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, successStatus(c.method, c.path), "successStatus(%s, %s)", c.method, c.path)
	}
}

func TestEnvRoundTrip(t *testing.T) {
	in := map[string]string{"B": "2", "A": "1", "C": "x=y"}
	got := envToMap(mapToEnv(in))
	require.Len(t, got, len(in), "round-trip changed size: %v -> %v", in, got)
	for k, v := range in {
		assert.Equal(t, v, got[k], "key %q", k)
	}
	// mapToEnv sorts keys, so output is deterministic.
	assert.Equal(t, "A=1\nB=2\nC=x=y", mapToEnv(in), "mapToEnv")
}
