package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/cli"
)

// Regression tests for the code-review findings.

// backupTarget must not match another stack's db by bare name.
func TestBackupTargetScopesDBNameToLinkedStack(t *testing.T) {
	var backupsPath string
	rt, out, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/dbs":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "db-a", "name": "main", "stack_id": "stack-A"},
				{"id": "db-b", "name": "main", "stack_id": "stack-B"},
			})
		case strings.HasPrefix(r.URL.Path, "/api/v1/tiles/") && strings.HasSuffix(r.URL.Path, "/backups"):
			backupsPath = r.URL.Path
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "b1", "kind": "dump", "cron": "@daily", "destination_id": "d1", "keep_latest": 0, "enabled": true},
			})
		default:
			http.NotFound(w, r)
		}
	})
	rt.Link = func() (cli.Link, string, error) { return cli.Link{Stack: "stack-B"}, "", nil }
	require.NoError(t, run(t, rt, "backup", "ls", "main"))
	assert.Equal(t, "/api/v1/tiles/db-b/backups", backupsPath, "must pick the linked stack's db, not stack-A's")
	// piped shape keeps the old CLI's keep=/enabled= prefixes
	assert.Contains(t, out.String(), "keep=all")
	assert.Contains(t, out.String(), "enabled=true")
}

// resolveTile must propagate API failures, not report them as "no tile".
func TestResolveTilePropagatesAPIError(t *testing.T) {
	rt, _, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":401,"message":"bad key"}}`, http.StatusUnauthorized)
	})
	_, _, err := resolveTile(context.Background(), rt, mustClient(t, rt), "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad key")
	assert.NotContains(t, err.Error(), "no tile")
}

func mustClient(t *testing.T, rt *Runtime) *cli.Client {
	t.Helper()
	c, err := rt.Client()
	require.NoError(t, err)
	return c
}

// domain ls piped output: trailing env-on-default column only when set.
func TestDomainLsPipedTrailingColumn(t *testing.T) {
	rt, out, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "r1", "level": "stack", "owner_id": "s1", "host": "a.example", "include_env_on_default": true},
			{"id": "r2", "level": "org", "owner_id": "o1", "host": "b.example", "include_env_on_default": false},
		})
	})
	require.NoError(t, run(t, rt, "domain", "ls"))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "r1\tstack\ts1\ta.example\tenv-on-default", lines[0])
	assert.Equal(t, "r2\torg\to1\tb.example", lines[1])
}

// link --env with an env-less stack must error, not silently link without it.
func TestLinkEnvFlagOnEnvlessStack(t *testing.T) {
	rt, _, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/stacks":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "s1", "name": "solo"}})
		case strings.HasSuffix(r.URL.Path, "/envs"):
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		default:
			http.NotFound(w, r)
		}
	})
	err := run(t, rt, "link", "--env", "e-typo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "e-typo")
}

// --tile is the flag; --app must keep working as a silent alias.
func TestAppFlagAliasesTile(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/apps/t1/variables/resolved" {
			_ = json.NewEncoder(w).Encode(map[string]any{"variables": []any{}})
			return
		}
		http.NotFound(w, r)
	}
	for _, flag := range []string{"--tile", "--app"} {
		rt, _, _ := testRuntime(t, handler)
		require.NoError(t, run(t, rt, "vars", "pull", flag, "t1"), flag)
	}
}

// infra rm --force skips the confirm without mutating rt.Yes.
func TestInfraRmForceSkipsConfirm(t *testing.T) {
	deleted := false
	rt, out, _ := testRuntime(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/resolve":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"kind": "instance", "id": "i1", "name": "pg", "path": "org:stack:pg",
			})
		case strings.HasSuffix(r.URL.Path, "/provisions"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodDelete:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	require.NoError(t, run(t, rt, "infra", "rm", "org:stack:pg", "--force"),
		"--force must skip the confirm on non-TTY stdin")
	assert.True(t, deleted)
	assert.False(t, rt.Yes, "--force must not mutate the global rt.Yes")
	assert.Contains(t, out.String(), "Removed org:stack:pg")
}
