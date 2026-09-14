package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/cli"
)

// testRuntime wires a Runtime at a fake API server and captures IO.
func testRuntime(t *testing.T, handler http.HandlerFunc) (*Runtime, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	var out, errb bytes.Buffer
	rt := &Runtime{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errb,
		Client: func() (*cli.Client, error) {
			return cli.NewClient(cli.Config{URL: srv.URL, Key: "k"}), nil
		},
		Link:   func() (cli.Link, string, error) { return cli.Link{}, "", nil },
		Getenv: func(string) string { return "" },
	}
	return rt, &out, &errb
}

func run(t *testing.T, rt *Runtime, args ...string) error {
	t.Helper()
	root := NewRoot(rt)
	root.SetArgs(args)
	return root.Execute()
}

func stacksHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/stacks":
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"id": "s1", "name": "alpha"},
				{"id": "s2", "name": "beta"},
			})
		default:
			http.NotFound(w, r)
		}
	}
}

func TestStackLsTSV(t *testing.T) {
	rt, out, _ := testRuntime(t, stacksHandler(t))
	require.NoError(t, run(t, rt, "stack", "ls"))
	assert.Equal(t, "s1\talpha\ts2\tbeta\t", strings.ReplaceAll(out.String(), "\n", "\t"))
}

func TestStackLsJSON(t *testing.T) {
	rt, out, _ := testRuntime(t, stacksHandler(t))
	require.NoError(t, run(t, rt, "stack", "ls", "--json"))
	var rows []stackRow
	require.NoError(t, json.Unmarshal(out.Bytes(), &rows), "stdout not JSON: %s", out.String())
	require.Len(t, rows, 2)
	assert.Equal(t, "s1", rows[0].ID)
}

func TestLegacyStacksSpelling(t *testing.T) {
	rt, out, _ := testRuntime(t, stacksHandler(t))
	require.NoError(t, run(t, rt, "stacks"))
	assert.Contains(t, out.String(), "s1\talpha")
}

func TestUsageErrorExitCode(t *testing.T) {
	rt, _, _ := testRuntime(t, stacksHandler(t))
	err := run(t, rt, "stack", "ls", "--nope")
	require.Error(t, err)
	assert.Equal(t, 2, ExitCode(err))
}

func TestRmRefusesNonTTYWithoutYes(t *testing.T) {
	rt, _, _ := testRuntime(t, stacksHandler(t))
	err := run(t, rt, "stack", "rm", "s1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--yes", "refusal must name the skip flag")
}

func TestRmWithYes(t *testing.T) {
	deleted := false
	rt, out, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/v1/stacks/s1" {
			deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	})
	require.NoError(t, run(t, rt, "stack", "rm", "s1", "--yes"))
	assert.True(t, deleted, "DELETE not sent")
	assert.Contains(t, out.String(), "Removed s1")
}

func TestJSONErrorObject(t *testing.T) {
	rt, _, errb := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusBadGateway)
	})
	err := run(t, rt, "stack", "ls", "--json")
	require.Error(t, err)
	rt.EmitError(err) // what the fang error handler does
	var je jsonError
	require.NoError(t, json.Unmarshal(errb.Bytes(), &je), "stderr not a JSON error object: %s", errb.String())
	assert.NotEmpty(t, je.Error)
}

func TestJSONErrorCarriesCode(t *testing.T) {
	rt, _, errb := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"api key missing scope: backups:read"}}`))
	})
	err := run(t, rt, "stack", "ls", "--json")
	require.Error(t, err)
	rt.EmitError(err)
	var je jsonError
	require.NoError(t, json.Unmarshal(errb.Bytes(), &je), "stderr not JSON: %s", errb.String())
	assert.Equal(t, 403, je.Status)
	assert.Equal(t, "403", je.Code)
}

func TestVarsScopeExplicitNeedsSelector(t *testing.T) {
	rt, _, _ := testRuntime(t, stacksHandler(t))
	// --scope org without --org must refuse, never fall through to app scope
	err := run(t, rt, "vars", "get", "--scope", "org")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--org")
	assert.Equal(t, 2, ExitCode(err))
}

func TestEmitJSONNilSlice(t *testing.T) {
	rt, out, _ := testRuntime(t, stacksHandler(t))
	var s []stackRow
	require.NoError(t, rt.EmitJSON(s))
	assert.Equal(t, "[]", strings.TrimSpace(out.String()), "nil slice must render as []")
}

func TestEnvLsNeedsStack(t *testing.T) {
	rt, _, _ := testRuntime(t, stacksHandler(t))
	err := run(t, rt, "env", "ls")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--stack")
}
