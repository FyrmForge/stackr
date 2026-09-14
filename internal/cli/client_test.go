package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddVolumePayload guards the volume verb's path + JSON body.
func TestAddVolumePayload(t *testing.T) {
	var gotPath, gotMethod string
	var body map[string]any
	c, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"v1","name":"data","mount_path":"/data","volume_name":"stackr-vol-v1"}`))
	})
	defer srv.Close()

	v, err := c.AddVolume(context.Background(), "app1", "data", "/data")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/api/v1/apps/app1/volumes", gotPath)
	assert.Equal(t, "data", body["name"], "body = %+v", body)
	assert.Equal(t, "/data", body["mount_path"], "body = %+v", body)
	assert.Equal(t, "/data", v.MountPath, "volume = %+v", v)
}

// TestCreateAppPayload guards the JSON the CLI sends for the richest create verb:
// path, method, cron fields, env map, and omitempty (empty image/branch dropped).
func TestCreateAppPayload(t *testing.T) {
	var gotPath, gotMethod string
	var body map[string]any
	c, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"a1","name":"nightly"}`))
	})
	defer srv.Close()

	app, err := c.CreateApp(context.Background(), "p1", AppCreate{
		Name: "nightly", Kind: "cron", GitURL: "git@x:y.git",
		Schedule: "0 3 * * *", TimeoutMinutes: 10, Env: map[string]string{"K": "V"},
	})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/api/v1/stacks/p1/apps", gotPath)
	assert.Equal(t, "cron", body["kind"], "cron fields = %+v", body)
	assert.Equal(t, "0 3 * * *", body["schedule"], "cron fields = %+v", body)
	_, hasImage := body["image"]
	assert.False(t, hasImage, "empty image should be omitted")
	env, _ := body["env"].(map[string]any)
	assert.Equal(t, "V", env["K"], "env = %+v", body["env"])
	assert.Equal(t, "a1", app.ID, "app = %+v", app)
}

func testClient(h http.HandlerFunc) (*Client, *httptest.Server) {
	srv := httptest.NewServer(h)
	return NewClient(Config{URL: srv.URL, Key: "sk_test"}), srv
}

func TestAppsQueryAndAuth(t *testing.T) {
	var gotKey, gotStack, gotEnv string
	c, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotStack = r.URL.Query().Get("stack")
		gotEnv = r.URL.Query().Get("env")
		_, _ = w.Write([]byte(`[{"id":"a1","name":"api","env_id":"e1","status":"running"}]`))
	})
	defer srv.Close()

	apps, err := c.Apps(context.Background(), "p1", "e1")
	require.NoError(t, err)
	assert.Equal(t, "sk_test", gotKey, "x-api-key = %q", gotKey)
	assert.Equal(t, "p1", gotStack, "query stack=%q env=%q, want p1/e1", gotStack, gotEnv)
	assert.Equal(t, "e1", gotEnv, "query stack=%q env=%q, want p1/e1", gotStack, gotEnv)
	require.Len(t, apps, 1, "apps = %+v", apps)
	assert.Equal(t, "api", apps[0].Name, "apps = %+v", apps)
}

func TestDeploy(t *testing.T) {
	c, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/apps/a1/deploy") {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"deployment":"d1"}`))
	})
	defer srv.Close()

	id, err := c.Deploy(context.Background(), "a1")
	require.NoError(t, err)
	assert.Equal(t, "d1", id, "deployment id = %q, want d1", id)
}

func TestLogs(t *testing.T) {
	c, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"lines":"line one\nline two"}`))
	})
	defer srv.Close()

	out, err := c.Logs(context.Background(), "a1", 50)
	require.NoError(t, err)
	assert.Equal(t, "line one\nline two", out, "logs = %q", out)
}

func TestFollowLogsParsesSSE(t *testing.T) {
	c, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		// stdout, stderr, a heartbeat comment, and an unmarked line.
		_, _ = w.Write([]byte("data: O out1\n\ndata: E err1\n\n: ping\n\ndata: bare\n\n"))
	})
	defer srv.Close()

	var events []LogEvent
	require.NoError(t, c.FollowLogs(context.Background(), "a1", 10, func(ev LogEvent) error {
		events = append(events, ev)
		return nil
	}))
	// O/E markers stripped into streams, heartbeat dropped, unmarked line kept.
	assert.Equal(t, []LogEvent{
		{Stream: "stdout", Line: "out1"},
		{Stream: "stderr", Line: "err1"},
		{Stream: "stdout", Line: "bare"},
	}, events)
}

func TestFollowLogsNotRunning(t *testing.T) {
	c, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	defer srv.Close()

	n := 0
	err := c.FollowLogs(context.Background(), "a1", 10, func(LogEvent) error { n++; return nil })
	assert.NoError(t, err, "204 should be a clean no-op, got %v", err)
	assert.Equal(t, 0, n, "expected no events")
}

// SetVars merges rather than replacing: the endpoint is a whole-set PUT, so
// setting one variable must not delete the others.
func TestSetVarsMerges(t *testing.T) {
	var sent varsBody
	c, srv := testClient(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"variables":[{"name":"KEEP","value":"1"},{"name":"PORT","value":"80"}]}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sent)
		_, _ = w.Write(b)
	})
	defer srv.Close()

	_, err := c.SetVarsAt(context.Background(), VarScope{App: "app1"},
		[]Var{{Name: "PORT", Value: "8080"}, {Name: "TOKEN", Value: "s", Secret: true}})
	require.NoError(t, err)
	got := map[string]Var{}
	for _, v := range sent.Vars {
		got[v.Name] = v
	}
	require.Len(t, got, 3, "sent %d variables, want 3 (merge, not replace): %+v", len(got), sent.Vars)
	assert.Equal(t, "8080", got["PORT"].Value, "merged set wrong: %+v", got)
	assert.Equal(t, "1", got["KEEP"].Value, "merged set wrong: %+v", got)
	assert.True(t, got["TOKEN"].Secret, "merged set wrong: %+v", got)
}

// A multiline or quoted value has to survive the dotenv round trip.
func TestDotenvQuoting(t *testing.T) {
	cases := map[string]string{
		"8080":               "8080",
		"":                   `""`,
		"a b":                `"a b"`,
		"line1\nline2":       `"line1\nline2"`,
		`say "hi"`:           `"say \"hi\""`,
		`back\slash`:         `"back\\slash"`,
		"postgres://u:p@h/d": "postgres://u:p@h/d",
	}
	for in, want := range cases {
		assert.Equal(t, want, DotenvQuote(in), "DotenvQuote(%q)", in)
	}
}
