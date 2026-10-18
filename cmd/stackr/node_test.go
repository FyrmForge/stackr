package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeErr is a server that answers every request with status and body.
func fakeErr(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
}

const nodeJSON = `{"id":"n1","name":"vps-fsn1","role":"worker","state":"online","tags":["ingress","egress"],"envs":2,` +
	`"placements":[],"cpu":2,"mem_total":4294967296,"disk_free":76235669504,"docker":"29.9.0","arch":"amd64","version":"v0.5.0","behind":true,` +
	`"env_list":[{"id":"e1","path":"acme/shop/prod","org":"acme","volumes":[{"id":"v1","name":"pg-data","last_backup":"2026-10-12T08:00:00Z"},{"id":"v2","name":"cache","last_backup":null}]}],` +
	`"exit_for":["acme/shop/prod"],"ingress_for":[],"default_ingress":false}`

// node add asks for a token, prints the command, and refuses an unknown tag
// before any request.
func TestNodeAdd(t *testing.T) {
	r := fake(t, map[string]string{"/admin/join-tokens": `{"node_name":"vps-fsn1","command":"stackr-install join --panel https://p --token T --ca-sha256 F",` +
		`"one_line":"curl -fsSL https://p/join/T | sh","expires_at":"2099-01-01T00:00:00Z"}`})
	code, out, errw := cli(t, "node", "add", "vps-fsn1", "--tag", "ingress", "--tag", "egress")
	if code != 0 || !strings.Contains(out, "curl -fsSL https://p/join/T | sh") || !strings.Contains(out, "works once") ||
		strings.Contains(out, "trusted certificate") {
		t.Fatalf("add = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != `POST /api/v1/admin/join-tokens {"node_name":"vps-fsn1","tags":["ingress","egress"]}` {
		t.Errorf("writes = %v", w)
	}
	// a panel without a trusted certificate gets the two-step command
	r = fake(t, map[string]string{"/admin/join-tokens": `{"two_step":"curl -fLo stackr-install X\n./stackr-install join","expires_at":"2099-01-01T00:00:00Z"}`})
	code, out, errw = cli(t, "node", "add", "vps-fsn1", "--tag", "storage")
	if code != 0 || !strings.Contains(out, "no trusted certificate") || !strings.Contains(out, "  ./stackr-install join") || strings.Contains(out, "| sh") {
		t.Fatalf("add two-step = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || !strings.Contains(w[0], `"tags":["storage"]`) {
		t.Errorf("writes = %v", w)
	}
	// access is per placement now: the org flag is gone
	if code, _, errw = cli(t, "node", "add", "x", "--org", "acme"); code != 2 || !strings.Contains(errw, "unknown flag") {
		t.Errorf("add --org = %d %q", code, errw)
	}
	r = fake(t, nil)
	if code, _, errw := cli(t, "node", "add", "x", "--tag", "gpu"); code != 2 || !strings.Contains(errw, `unknown tag "gpu"`) || len(r.reqs) != 0 {
		t.Errorf("unknown tag = %d %q, sent %v", code, errw, r.reqs)
	}
	fake(t, map[string]string{"/admin/join-tokens": `{"node_name":"n","command":"c","expires_at":"2099-01-01T00:00:00Z"}`})
	if code, out, _ := cli(t, "--json", "node", "add", "n"); code != 0 || !strings.Contains(out, `"command": "c"`) {
		t.Errorf("--json = %d %q", code, out)
	}
}

// confirm looks the node up by name, asks, and posts the confirm for its id.
func TestNodeConfirm(t *testing.T) {
	pend := `[{"node_id":"abc","name":"vps-fsn1","tags":[],"facts":{"cpus":2,"mem_bytes":4294967296}}]`
	r := fake(t, map[string]string{"/admin/join-requests": pend, "/confirm": `{"id":"abc"}`})
	if code, _, errw := cli(t, "node", "confirm", "vps-fsn1"); code != 3 || !strings.Contains(errw, "Add vps-fsn1 (2 CPU, 4.0 GB)") || len(writes(r)) != 0 {
		t.Errorf("no -y = %d %q, writes %v", code, errw, writes(r))
	}
	if code, out, errw := cli(t, "-y", "node", "confirm", "vps-fsn1"); code != 0 || !strings.Contains(out, "vps-fsn1: confirmed") {
		t.Fatalf("-y = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "POST /api/v1/admin/join-requests/abc/confirm " {
		t.Errorf("writes = %v", w)
	}
	fake(t, map[string]string{"/admin/join-requests": `[]`, "/nodes/vps-ams": `{"name":"vps-ams","state":"online"}`})
	if code, _, errw := cli(t, "-y", "node", "confirm", "vps-ams"); code != 1 || !strings.Contains(errw, "already online") {
		t.Errorf("online = %d %q", code, errw)
	}
	fake(t, map[string]string{"/admin/join-requests": `[]`})
	if code, _, errw := cli(t, "-y", "node", "confirm", "zz"); code != 1 || !strings.Contains(errw, `no pending node "zz"`) {
		t.Errorf("unknown = %d %q", code, errw)
	}
}

func TestNodeReject(t *testing.T) {
	r := fake(t, map[string]string{"/admin/join-requests": `[{"node_id":"abc","name":"w"}]`})
	if code, _, _ := cli(t, "node", "reject", "w"); code != 3 {
		t.Errorf("no -y = %d, want 3", code)
	}
	if code, _, errw := cli(t, "-y", "node", "reject", "w"); code != 0 {
		t.Fatalf("-y = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || !strings.HasPrefix(w[0], "POST /api/v1/admin/join-requests/abc/reject") {
		t.Errorf("writes = %v", w)
	}
}

func TestNodeLs(t *testing.T) {
	// /nodes never lists a pending node: a waiting worker is a join request.
	pend := `[{"node_id":"n2","name":"nas","tags":["builder"],"facts":{"cpus":4,"mem_bytes":8589934592},"requested_at":"2026-10-16T10:00:00Z"}]`
	fake(t, map[string]string{"/nodes": `[` + nodeJSON + `]`, "/admin/join-requests": pend})
	code, out, errw := cli(t, "node", "ls")
	if code != 0 || !strings.Contains(out, "vps-fsn1\tworker\tonline\tingress,egress\t2\t2\t4.0 GB\t71 GB\t29.9.0") || strings.Contains(out, "nas") {
		t.Fatalf("ls = %d %q %q", code, out, errw)
	}
	for _, args := range [][]string{{"node", "ls", "--pending"}, {"node", "ls", "--state", "pending"}} {
		if _, out, _ = cli(t, args...); strings.Contains(out, "vps-fsn1") || !strings.Contains(out, "nas\tbuilder\t4\t8.0 GB\t2026-10-16T10:00:00Z") {
			t.Errorf("%v = %q", args, out)
		}
	}
	if _, out, _ = cli(t, "node", "ls", "--state", "online"); strings.Contains(out, "nas") || !strings.Contains(out, "vps-fsn1") {
		t.Errorf("--state = %q", out)
	}
	if _, out, _ = cli(t, "--json", "node", "ls", "--pending"); !strings.Contains(out, `"name": "nas"`) || strings.Contains(out, "vps-fsn1") {
		t.Errorf("--json = %q", out)
	}
	// A worker the tunnel misses but whose host answers reads as the panel says.
	fake(t, map[string]string{"/nodes": strings.Replace(`[`+nodeJSON+`]`, `"online"`, `"tunnel-down"`, 1),
		"/nodes/vps-fsn1": strings.Replace(nodeJSON, `"online"`, `"tunnel-down"`, 1)})
	if _, out, _ = cli(t, "node", "ls"); !strings.Contains(out, "\ttunnel down, host alive\t") {
		t.Errorf("tunnel down ls = %q", out)
	}
	if _, out, _ = cli(t, "node", "show", "vps-fsn1"); !strings.Contains(out, "state\ttunnel down, host alive") {
		t.Errorf("tunnel down show = %q", out)
	}
}

func TestNodeShow(t *testing.T) {
	fake(t, map[string]string{"/nodes/vps-fsn1": nodeJSON})
	code, out, errw := cli(t, "node", "show", "vps-fsn1")
	if code != 0 || !strings.Contains(out, "tags\tingress, egress") || !strings.Contains(out, "arch\tamd64") || !strings.Contains(out, "stackr\tv0.5.0 (behind)") || !strings.Contains(out, "envs\tacme/shop/prod") || !strings.Contains(out, "exit for\tacme/shop/prod") {
		t.Errorf("show = %d %q %q", code, out, errw)
	}
	fake(t, map[string]string{"/nodes/vps-fsn1": strings.Replace(nodeJSON, `"behind":true,`, `"behind":true,"too_old":true,`, 1)})
	if _, out, _ = cli(t, "node", "show", "vps-fsn1"); !strings.Contains(out, "stackr\tv0.5.0 (too old)") {
		t.Errorf("too old show = %q", out)
	}
	if code, _, errw = cli(t, "node", "show"); code != 2 {
		t.Errorf("no name = %d %q", code, errw)
	}
}

// ls has a WARN column of short labels; show lists each warning in full.
func TestNodeWarnings(t *testing.T) {
	warned := strings.Replace(nodeJSON, `"behind":true,`, `"behind":true,"warnings":[`+
		`{"kind":"clock","severity":"warning","label":"Clock 2m ahead","detail":"Its clock is 2m ahead of the panel."},`+
		`{"kind":"disk","severity":"warning","label":"Disk low","detail":"0.5 GB (3%) free on its Docker disk."}],`, 1)
	fake(t, map[string]string{"/nodes": `[` + nodeJSON + `]`})
	if _, out, _ := cli(t, "node", "ls"); !strings.Contains(out, "29.9.0\t-") {
		t.Errorf("no warnings ls = %q", out)
	}
	fake(t, map[string]string{"/nodes": `[` + warned + `]`, "/nodes/vps-fsn1": warned})
	if _, out, _ := cli(t, "node", "ls"); !strings.Contains(out, "29.9.0\tClock 2m ahead, Disk low") {
		t.Errorf("ls = %q", out)
	}
	code, out, errw := cli(t, "node", "show", "vps-fsn1")
	if code != 0 || !strings.Contains(out, "warning\tIts clock is 2m ahead of the panel.") || !strings.Contains(out, "warning\t0.5 GB (3%) free") {
		t.Errorf("no name = %d %q", code, errw)
	}
}

// tag sends the adds; untag of an exit asks first, naming what loses it.
func TestNodeTagUntag(t *testing.T) {
	r := fake(t, map[string]string{"/tags": `{"tags":["ingress","egress","builder"]}`, "/nodes/vps-fsn1": nodeJSON})
	code, out, errw := cli(t, "node", "tag", "vps-fsn1", "builder")
	if code != 0 || !strings.Contains(out, "vps-fsn1: tags ingress, egress, builder") {
		t.Fatalf("tag = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != `PUT /api/v1/admin/nodes/vps-fsn1/tags {"add":["builder"],"remove":[]}` {
		t.Errorf("writes = %v", w)
	}
	if code, _, errw = cli(t, "node", "tag", "vps-fsn1", "gpu"); code != 2 || !strings.Contains(errw, "unknown tag") {
		t.Errorf("unknown tag = %d %q", code, errw)
	}
	r = fake(t, map[string]string{"/tags": `{"tags":["ingress"]}`, "/nodes/vps-fsn1": nodeJSON})
	if code, _, errw = cli(t, "node", "untag", "vps-fsn1", "egress"); code != 3 || !strings.Contains(errw, "is the exit for 1 scope: acme/shop/prod") || len(writes(r)) != 0 {
		t.Errorf("untag no -y = %d %q, writes %v", code, errw, writes(r))
	}
	if code, _, errw = cli(t, "-y", "node", "untag", "vps-fsn1", "egress"); code != 0 {
		t.Fatalf("untag -y = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != `PUT /api/v1/admin/nodes/vps-fsn1/tags {"add":[],"remove":["egress"]}` {
		t.Errorf("untag writes = %v", w)
	}
}

// exit sends the names; none clears; show prints the default exit.
func TestNodeExit(t *testing.T) {
	r := fake(t, map[string]string{"/exit": `{"default_exit":["vps-fsn1"]}`})
	code, out, errw := cli(t, "node", "exit", "home", "vps-fsn1", "vps-hel1")
	if code != 0 || !strings.Contains(out, "home: default exit vps-fsn1, vps-hel1") {
		t.Fatalf("exit = %d %q %q", code, out, errw)
	}
	if code, out, _ = cli(t, "node", "exit", "home"); code != 0 || !strings.Contains(out, "home: default exit cleared") {
		t.Errorf("clear = %d %q", code, out)
	}
	if w := writes(r); len(w) != 2 || w[0] != `PUT /api/v1/admin/nodes/home/exit {"exits":["vps-fsn1","vps-hel1"]}` || w[1] != `PUT /api/v1/admin/nodes/home/exit {"exits":[]}` {
		t.Errorf("writes = %v", w)
	}
	if code, _, _ = cli(t, "node", "exit"); code != 2 {
		t.Errorf("no name = %d", code)
	}
	fake(t, map[string]string{"/nodes/home": `{"name":"home","role":"worker","state":"online","tags":["builder"],"default_exit":["vps-fsn1"],"env_list":[]}`})
	if _, out, _ = cli(t, "node", "show", "home"); !strings.Contains(out, "default exit\tvps-fsn1") {
		t.Errorf("show = %q", out)
	}
	fakeErr(t, 409, `{"error":"Pick a default exit for home first."}`)
	if code, _, errw = cli(t, "node", "exit", "home"); code == 0 || !strings.Contains(errw, "Pick a default exit for home first.") {
		t.Errorf("refusal = %d %q", code, errw)
	}
}

func TestNodeRejoin(t *testing.T) {
	r := fake(t, map[string]string{"/rejoin": `{"command":"stackr-install join --token R","one_line":"curl -fsSL https://p/join/R | sh","expires_at":"2099-01-01T00:00:00Z"}`})
	code, out, errw := cli(t, "node", "rejoin", "nas")
	if code != 0 || !strings.Contains(out, "nas will need its install command run again") || !strings.Contains(out, "/join/R | sh") {
		t.Fatalf("rejoin = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "POST /api/v1/admin/nodes/nas/rejoin " {
		t.Errorf("writes = %v", w)
	}
}

func TestNodeRemove(t *testing.T) {
	empty := `{"name":"w","env_list":[]}`
	r := fake(t, map[string]string{"/nodes/w": empty})
	if code, _, _ := cli(t, "node", "remove", "w"); code != 3 || len(writes(r)) != 0 {
		t.Errorf("no -y = %d, writes %v", code, writes(r))
	}
	// the answer is a job; this fake's has no id, so it is shown, not followed
	if code, out, errw := cli(t, "-y", "node", "remove", "w"); code != 0 {
		t.Fatalf("-y = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "DELETE /api/v1/admin/nodes/w " {
		t.Errorf("writes = %v", w)
	}
	// a placement on it, or envs on it: refused, nothing sent
	r = fake(t, map[string]string{"/nodes/w": strings.Replace(nodeJSON, `"placements":[]`, `"placements":["eu-prod"]`, 1)})
	if code, _, errw := cli(t, "-y", "node", "remove", "w"); code != 1 || !strings.Contains(errw, "hosts placement eu-prod") || len(writes(r)) != 0 {
		t.Errorf("with placement = %d %q, writes %v", code, errw, writes(r))
	}
	r = fake(t, map[string]string{"/nodes/w": nodeJSON})
	if code, _, errw := cli(t, "-y", "node", "remove", "w"); code != 1 || !strings.Contains(errw, "runs 1 env. Repoint its placements first") || len(writes(r)) != 0 {
		t.Errorf("with envs = %d %q, writes %v", code, errw, writes(r))
	}
	// the controller is refused, envs or not
	r = fake(t, map[string]string{"/nodes/w": strings.Replace(nodeJSON, `"role":"worker"`, `"role":"controller"`, 1)})
	if code, _, errw := cli(t, "-y", "node", "remove", "w"); code != 1 || !strings.Contains(errw, "is the controller") || len(writes(r)) != 0 {
		t.Errorf("controller = %d %q, writes %v", code, errw, writes(r))
	}
}

func TestNodeForget(t *testing.T) {
	r := fake(t, map[string]string{"/nodes/nas": nodeJSON, "/forget": `{"node":"nas","lost":[{"path":"acme/shop/prod"}]}`})
	code, _, errw := cli(t, "node", "forget", "nas")
	if code != 3 || !strings.Contains(errw, "acme/shop/prod") || !strings.Contains(errw, "volumes pg-data (last backup") || !strings.Contains(errw, "cache (no backup)") || len(writes(r)) != 0 {
		t.Errorf("no -y = %d %q, writes %v", code, errw, writes(r))
	}
	code, out, errw := cli(t, "-y", "node", "forget", "nas")
	if code != 0 || !strings.Contains(out, "nas: forgotten; 1 env marked lost") || !strings.Contains(out, "stackr env restore") {
		t.Fatalf("-y = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "POST /api/v1/admin/nodes/nas/forget " {
		t.Errorf("writes = %v", w)
	}
}

func TestPin(t *testing.T) {
	r := fake(t, map[string]string{"/pin": `{"scope":"acme","placement":"eu-prod"}`})
	code, out, errw := cli(t, "pin", "eu-prod", "--org", "acme")
	if code != 0 || !strings.Contains(out, "acme: envs go to eu-prod") {
		t.Fatalf("org pin = %d %q %q", code, out, errw)
	}
	if code, _, errw = cli(t, "pin", "eu-prod", "--stack", "shop"); code != 0 {
		t.Fatalf("stack pin = %d %q", code, errw)
	}
	if code, out, _ = cli(t, "pin", "--clear", "--stack", "shop"); code != 0 || !strings.Contains(out, "pin removed") {
		t.Errorf("clear = %d %q", code, out)
	}
	w := writes(r)
	want := []string{
		`PUT /api/v1/orgs/acme/pin {"placement":"eu-prod"}`,
		`PUT /api/v1/orgs/acme/stacks/shop/pin {"placement":"eu-prod"}`,
		`PUT /api/v1/orgs/acme/stacks/shop/pin {"placement":""}`,
	}
	if strings.Join(w, "|") != strings.Join(want, "|") {
		t.Errorf("writes = %v", w)
	}
	if code, _, _ = cli(t, "pin", "eu-prod", "--clear", "--org", "acme"); code != 2 {
		t.Errorf("placement and clear = %d", code)
	}
	// a server refusal (a placement not open to the org) is exit 1 with the words
	fakeErr(t, 403, `{"error":"eu-db is not open to acme"}`)
	if code, _, errw = cli(t, "pin", "eu-db", "--stack", "shop"); code != 1 || !strings.Contains(errw, "eu-db is not open to acme") {
		t.Errorf("refusal = %d %q", code, errw)
	}
	// bare pin shows where new envs go
	fake(t, map[string]string{"/pin": `{"scope":"acme","placement":""}`})
	if code, out, _ = cli(t, "pin", "--org", "acme"); code != 0 || !strings.Contains(out, "no pin; new envs go to the default placement") {
		t.Errorf("show = %d %q", code, out)
	}
}

// An env pin that moves the env prints the plan and needs --yes; one that
// moves nothing is a plain set.
func TestPinEnv(t *testing.T) {
	const plan = `{"placement":"eu-prod","from":"eu-db","to":"eu-prod","envs":[{"path":"acme/shop/prod","volumes":1,"bytes":1073741824,"downtime":"about 20 s"}],` +
		`"checks":[{"name":"tags","ok":true},{"name":"disk","ok":true,"detail":"h4 free 12 GB"}],"ok":true}`
	args := []string{"pin", "eu-prod", "--stack", "shop", "--env", "prod"}
	r := fake(t, map[string]string{"/pin-plan": plan, "/pin": `{"scope":"acme/shop/prod","placement":"eu-prod","job":{}}`})
	code, out, errw := cli(t, args...)
	if code != 3 || !strings.Contains(out, "Move eu-prod: eu-db to eu-prod") || !strings.Contains(out, "acme/shop/prod") ||
		!strings.Contains(out, "ok    tags") || len(writes(r)) != 0 {
		t.Errorf("no -y = %d %q %q, writes %v", code, out, errw, writes(r))
	}
	if code, _, errw = cli(t, append([]string{"-y"}, args...)...); code != 0 {
		t.Fatalf("-y = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != `PUT /api/v1/orgs/acme/stacks/shop/envs/prod/pin {"confirm":"prod","placement":"eu-prod"}` {
		t.Errorf("writes = %v", w)
	}
	// a failing check stops before the prompt
	r = fake(t, map[string]string{"/pin-plan": strings.Replace(strings.Replace(plan, `],"ok":true}`, `],"ok":false}`, 1), `"name":"tags","ok":true`, `"name":"tags","ok":false`, 1)})
	if code, out, errw = cli(t, append([]string{"-y"}, args...)...); code != 1 || !strings.Contains(out, "FAIL  tags") || len(writes(r)) != 0 {
		t.Errorf("failed check = %d %q %q, writes %v", code, out, errw, writes(r))
	}
	// no move: a plain set
	r = fake(t, map[string]string{"/pin-plan": `{"envs":[],"checks":[],"ok":true}`, "/pin": `{"scope":"acme/shop/prod","placement":"eu-prod"}`})
	if code, out, errw = cli(t, args...); code != 0 || !strings.Contains(out, "acme/shop/prod: envs go to eu-prod") {
		t.Fatalf("plain = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != `PUT /api/v1/orgs/acme/stacks/shop/envs/prod/pin {"placement":"eu-prod"}` {
		t.Errorf("plain writes = %v", w)
	}
}

// A stack or org pin that moves envs prints one plan per env and needs --yes,
// typing the scope's slug; the moves it starts are followed.
func TestPinStackMoves(t *testing.T) {
	const plans = `[{"placement":"acme/shop/dev","from":"default","to":"eu-prod","envs":[{"path":"acme/shop/dev","volumes":0,"bytes":0,"downtime":"about 5 s"}],"checks":[{"name":"tags","ok":true}],"ok":true},` +
		`{"placement":"acme/shop/prod","from":"default","to":"eu-prod","envs":[{"path":"acme/shop/prod","volumes":1,"bytes":10,"downtime":"about 20 s"}],"checks":[{"name":"tags","ok":true}],"ok":true}]`
	args := []string{"pin", "eu-prod", "--stack", "shop"}
	r := fake(t, map[string]string{"/pin-plan": plans, "/pin": `{"scope":"acme/shop","placement":"eu-prod","jobs":[{},{}]}`})
	code, out, errw := cli(t, args...)
	if code != 3 || !strings.Contains(out, "acme/shop/dev") || !strings.Contains(out, "acme/shop/prod") ||
		!strings.Contains(errw, "That moves 2 envs (acme/shop/dev, acme/shop/prod)") || len(writes(r)) != 0 {
		t.Errorf("no -y = %d %q %q, writes %v", code, out, errw, writes(r))
	}
	if code, _, errw = cli(t, append([]string{"-y"}, args...)...); code != 0 {
		t.Fatalf("-y = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != `PUT /api/v1/orgs/acme/stacks/shop/pin {"confirm":"shop","placement":"eu-prod"}` {
		t.Errorf("writes = %v", w)
	}
}

func TestIngress(t *testing.T) {
	r := fake(t, map[string]string{"/ingress": `{"scope":"acme/shop/prod","placement":"eu-prod","from":"acme/shop"}`})
	code, out, errw := cli(t, "ingress", "get", "--stack", "shop", "--env", "prod")
	if code != 0 || !strings.Contains(out, "acme/shop/prod\teu-prod\t(from acme/shop)") {
		t.Fatalf("get = %d %q %q", code, out, errw)
	}
	if code, _, errw = cli(t, "ingress", "set", "eu-prod", "--tile", "admin", "--stack", "shop", "--env", "prod"); code != 0 {
		t.Fatalf("set tile = %d %q", code, errw)
	}
	if code, _, errw = cli(t, "ingress", "set", "eu-prod", "--stack", "shop"); code != 0 {
		t.Fatalf("set stack = %d %q", code, errw)
	}
	if code, _, _ = cli(t, "ingress", "set", "--clear", "--org", "acme"); code != 0 {
		t.Fatalf("clear = %d", code)
	}
	want := []string{
		`PUT /api/v1/orgs/acme/stacks/shop/envs/prod/tiles/admin/ingress {"placement":"eu-prod"}`,
		`PUT /api/v1/orgs/acme/stacks/shop/ingress {"placement":"eu-prod"}`,
		`PUT /api/v1/orgs/acme/ingress {"placement":""}`,
	}
	if w := writes(r); strings.Join(w, "|") != strings.Join(want, "|") {
		t.Errorf("writes = %v", w)
	}
	if code, _, errw = cli(t, "ingress", "set"); code != 2 {
		t.Errorf("no placement = %d %q", code, errw)
	}
	fakeErr(t, 403, `{"error":"eu-db's node is not tagged ingress"}`)
	if code, _, errw = cli(t, "ingress", "set", "eu-db", "--stack", "shop"); code != 1 || !strings.Contains(errw, "not tagged ingress") {
		t.Errorf("untagged = %d %q", code, errw)
	}
}

func TestExit(t *testing.T) {
	r := fake(t, map[string]string{"/exits": `{"scope":"acme/shop/prod","from":"acme/shop/prod","exits":[{"position":1,"placement":"eu-prod","state":"in use"},{"position":2,"placement":"home","state":"standby"}]}`})
	code, out, errw := cli(t, "exit", "set", "eu-prod", "home", "--stack", "shop", "--env", "prod")
	if code != 0 || !strings.Contains(out, "acme/shop/prod: goes out through eu-prod, then home; no internet if all are down") {
		t.Fatalf("set = %d %q %q", code, out, errw)
	}
	if code, out, _ = cli(t, "exit", "set", "--clear", "--stack", "shop", "--env", "prod"); code != 0 || !strings.Contains(out, "its own placement") {
		t.Errorf("clear = %d %q", code, out)
	}
	want := []string{
		`PUT /api/v1/orgs/acme/stacks/shop/envs/prod/exits {"placements":["eu-prod","home"]}`,
		`PUT /api/v1/orgs/acme/stacks/shop/envs/prod/exits {"placements":[]}`,
	}
	if w := writes(r); strings.Join(w, "|") != strings.Join(want, "|") {
		t.Errorf("writes = %v", w)
	}
	code, out, errw = cli(t, "exit", "ls", "--stack", "shop", "--env", "prod")
	if code != 0 || !strings.Contains(out, "1\teu-prod\tin use") || !strings.Contains(out, "2\thome\tstandby") {
		t.Errorf("ls = %d %q %q", code, out, errw)
	}
	fake(t, map[string]string{"/exits": `{"scope":"acme/shop","from":"default","exits":[]}`})
	if _, out, _ = cli(t, "exit", "ls", "--stack", "shop"); !strings.Contains(out, "its own placement") {
		t.Errorf("empty ls = %q", out)
	}
	if code, _, _ = cli(t, "exit", "set", "a", "--clear"); code != 2 {
		t.Errorf("placements and clear = %d", code)
	}
}

// The removed verbs are gone, not hidden.
func TestNodeAccessVerbsGone(t *testing.T) {
	r := fake(t, nil)
	for _, args := range [][]string{{"node", "open", "x"}, {"node", "close", "x"}, {"env", "move", "--stack", "s", "--env", "e", "--to", "n"}} {
		if code, _, _ := cli(t, args...); code == 0 || len(r.reqs) != 0 {
			t.Errorf("%v = %d, sent %v", args, code, r.reqs)
		}
	}
}

// stackr placements is the member's list: names, tags, public IP, online,
// usable, and no node.
func TestPlacementsList(t *testing.T) {
	list := `[{"name":"eu-prod","tags":["ingress","egress"],"public":true,"online":true,"has_node":true,"usable":true},` +
		`{"name":"eu-db","tags":["storage"],"public":false,"online":false,"has_node":true,"usable":false,"why":"offline"}]`
	r := fake(t, map[string]string{"/placements": list})
	code, out, errw := cli(t, "placements")
	if code != 0 || !strings.Contains(out, "eu-prod\tingress,egress\tyes\tyes\tyes") || !strings.Contains(out, "eu-db\tstorage\tno\tno\tno\toffline") {
		t.Fatalf("placements = %d %q %q", code, out, errw)
	}
	if strings.Contains(out, "node") || len(r.reqs) != 1 || r.reqs[0] != "GET /api/v1/orgs/acme/placements " {
		t.Errorf("out %q, sent %v", out, r.reqs)
	}
	if code, out, _ = cli(t, "--json", "placements"); code != 0 || !strings.Contains(out, `"name": "eu-prod"`) {
		t.Errorf("json = %d %q", code, out)
	}
}

func TestAdminPlacement(t *testing.T) {
	r := fake(t, map[string]string{"/admin/placements": `[` +
		`{"name":"default","node":"ctl","tags":["ingress"],"online":true,"open_to_all":true,"open_to":[],"envs":3},` +
		`{"name":"eu-db","node":"h3","tags":["storage"],"online":false,"open_to_all":false,"open_to":["acme"],"envs":1},` +
		`{"name":"eu-new","node":"","tags":[],"online":false,"open_to_all":true,"open_to":[],"envs":0},` +
		`{"name":"old","node":"h9","online":false,"open_to_all":true,"open_to":[],"envs":2,"node_forgotten":true}]`})
	code, out, errw := cli(t, "admin", "placement", "ls")
	for _, w := range []string{"default\tctl\tingress\tall\t3\tonline", "eu-db\th3\tstorage\tacme\t1\toffline", "no node yet", "node forgotten: repoint to restore"} {
		if code != 0 || !strings.Contains(out, w) {
			t.Fatalf("ls = %d %q %q, want %q", code, out, errw, w)
		}
	}
	if code, out, errw = cli(t, "admin", "placement", "add", "eu-prod", "--node", "h4", "--open", "acme,shop"); code != 0 || !strings.Contains(out, "eu-prod: made, on h4") {
		t.Fatalf("add = %d %q %q", code, out, errw)
	}
	if code, _, _ = cli(t, "admin", "placement", "add", "eu-x", "--open", ","); code != 2 {
		t.Errorf("bad --open = %d", code)
	}
	if code, out, errw = cli(t, "admin", "placement", "open", "eu-prod", "all"); code != 0 || !strings.Contains(out, "open to all orgs") {
		t.Fatalf("open = %d %q %q", code, out, errw)
	}
	if code, _, _ = cli(t, "admin", "placement", "rm", "eu-x"); code != 3 {
		t.Errorf("rm no -y = %d", code)
	}
	if code, out, errw = cli(t, "-y", "admin", "placement", "rm", "eu-x"); code != 0 || !strings.Contains(out, "eu-x: deleted") {
		t.Fatalf("rm = %d %q %q", code, out, errw)
	}
	want := []string{
		`POST /api/v1/admin/placements {"name":"eu-prod","node":"h4","orgs":["acme","shop"]}`,
		`PUT /api/v1/admin/placements/eu-prod/open {"all":true,"orgs":null}`,
		`DELETE /api/v1/admin/placements/eu-x `,
	}
	if w := writes(r); strings.Join(w, "|") != strings.Join(want, "|") {
		t.Errorf("writes = %v", w)
	}
}

// move prints the checks; it exits 3 until --yes, which sends the placement's
// name as the confirm. A failing check stops it with no write.
func TestAdminPlacementMove(t *testing.T) {
	const plan = `{"placement":"eu-prod","from":"h4","to":"h5","restore":false,"envs":[{"path":"acme/shop/prod","volumes":2,"bytes":2147483648,"downtime":"about 25 s"}],` +
		`"checks":[{"name":"target online","ok":true},{"name":"disk","ok":true,"detail":"h5 free 40 GB, needs 2.0 GB + 10%"}],"ok":true}`
	r := fake(t, map[string]string{"/move-plan": plan, "/move": `{}`})
	code, out, errw := cli(t, "admin", "placement", "move", "eu-prod", "h5")
	if code != 3 || !strings.Contains(out, "Move eu-prod: h4 to h5") || !strings.Contains(out, "ok    target online") ||
		!strings.Contains(out, "h5 free 40 GB") || !strings.Contains(errw, "Move placement eu-prod from h4 to h5? Its env stops while it moves.") || len(writes(r)) != 0 {
		t.Errorf("no -y = %d %q %q, writes %v", code, out, errw, writes(r))
	}
	// the job is the first env's: the command waits for the repoint's own end
	r = fake(t, map[string]string{"/move-plan": plan, "/move": `{"state":"done","from":"h4","to":"h5","done":2,"all":2,` +
		`"envs":[{"path":"acme/shop/prod","active":false,"steps":[]},{"path":"acme/shop/dev","active":false,"error":"h5 disk full","steps":[]}]}`})
	if code, out, errw = cli(t, "-y", "admin", "placement", "move", "eu-prod", "h5"); code != 0 || !strings.Contains(out, "eu-prod: done to h5, 2 of 2 envs moved") ||
		!strings.Contains(out, "acme/shop/prod\tmoved") || !strings.Contains(out, "acme/shop/dev\tfailed: h5 disk full") {
		t.Fatalf("-y = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != `POST /api/v1/admin/placements/eu-prod/move {"confirm":"eu-prod","to":"h5"}` {
		t.Errorf("writes = %v", w)
	}
	bad := strings.Replace(strings.Replace(plan, `],"ok":true}`, `],"ok":false}`, 1), `"name":"disk","ok":true`, `"name":"disk","ok":false`, 1)
	r = fake(t, map[string]string{"/move-plan": bad})
	if code, out, errw = cli(t, "-y", "admin", "placement", "move", "eu-prod", "h5"); code != 1 || !strings.Contains(out, "FAIL  disk") || len(writes(r)) != 0 {
		t.Errorf("failed check = %d %q %q, writes %v", code, out, errw, writes(r))
	}
}

func TestAdminPlacementRetry(t *testing.T) {
	r := fake(t, map[string]string{"/move": `{"state":"failed","from":"h4","to":"h5","done":1,"all":2,"error":"h5 disk full"}`, "/retry": `{}`})
	// the fake's repoint stays failed, so the retry ends failed too
	code, out, errw := cli(t, "admin", "placement", "retry", "eu-prod")
	if code != 1 || !strings.Contains(out, "eu-prod: failed to h5, 1 of 2 envs moved") || !strings.Contains(out, "h5 disk full") || !strings.Contains(errw, "the move of eu-prod failed") {
		t.Fatalf("retry = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "POST /api/v1/admin/placements/eu-prod/retry " {
		t.Errorf("writes = %v", w)
	}
}

func TestEnvLostRestore(t *testing.T) {
	lost := `[{"id":"e1","path":"acme/api/dev","org":"acme","placement":"eu-db","volumes":[{"id":"v1","name":"db-data","run_id":"r1","last_backup":"2026-10-12T08:00:00Z"},{"id":"v2","name":"cache","last_backup":null}]}]`
	fake(t, map[string]string{"/lost-envs": lost})
	code, out, errw := cli(t, "env", "lost")
	if code != 0 || !strings.Contains(out, "acme/api/dev\teu-db\tdb-data,cache") {
		t.Fatalf("lost = %d %q %q", code, out, errw)
	}
	// a restore is a repoint off a forgotten node: the server restores the volumes
	const plan = `{"placement":"eu-db","from":"h3","to":"h5","restore":true,"envs":[{"path":"acme/api/dev","volumes":2,"bytes":-1,"downtime":"about 1 min"}],"checks":[{"name":"target online","ok":true}],"ok":true}`
	r := fake(t, map[string]string{"/move-plan": plan, "/move": `{"state":"done","from":"h3","to":"h5","done":1,"all":1}`})
	args := []string{"env", "restore", "--placement", "eu-db", "--to", "h5"}
	if code, out, errw = cli(t, args...); code != 3 || !strings.Contains(out, "Restore from backups eu-db") || !strings.Contains(errw, "Restore placement eu-db on h5?") || len(writes(r)) != 0 {
		t.Errorf("no -y = %d %q %q, writes %v", code, out, errw, writes(r))
	}
	if code, out, errw = cli(t, append([]string{"-y"}, args...)...); code != 0 || !strings.Contains(out, "eu-db: done to h5, 1 of 1 envs moved") {
		t.Fatalf("-y = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != `POST /api/v1/admin/placements/eu-db/move {"confirm":"eu-db","to":"h5"}` {
		t.Errorf("writes = %v", w)
	}
	// a volume with no backup: -y alone refuses; --start-empty acknowledges it
	empty := strings.Replace(strings.Replace(plan, `{"name":"target online","ok":true}`,
		`{"name":"target online","ok":true},{"name":"disk","ok":false,"overridable":true,"detail":"no backup for cache; it starts empty"}`, 1), `],"ok":true}`, `],"ok":false}`, 1)
	r = fake(t, map[string]string{"/move-plan": empty, "/move": `{"state":"done","from":"h3","to":"h5","done":1,"all":1}`})
	if code, out, errw = cli(t, append([]string{"-y"}, args...)...); code != 1 || !strings.Contains(out, "WARN  disk") ||
		!strings.Contains(errw, "no backup for cache; it starts empty; pass --start-empty") || len(writes(r)) != 0 {
		t.Errorf("no --start-empty = %d %q %q, writes %v", code, out, errw, writes(r))
	}
	if code, _, errw = cli(t, append(append([]string{}, args...), "--start-empty")...); code != 3 || !strings.Contains(errw, "no backup for cache; it starts empty. Restore placement eu-db") {
		t.Errorf("--start-empty, no -y = %d %q", code, errw)
	}
	if code, out, errw = cli(t, append(append([]string{"-y"}, args...), "--start-empty")...); code != 0 {
		t.Fatalf("--start-empty -y = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != `POST /api/v1/admin/placements/eu-db/move {"confirm":"eu-db","start_empty":true,"to":"h5"}` {
		t.Errorf("writes = %v", w)
	}
	// a placement that is not on a forgotten node is a plain move: refused here
	r = fake(t, map[string]string{"/move-plan": strings.Replace(plan, `"restore":true`, `"restore":false`, 1)})
	if code, _, errw = cli(t, append([]string{"-y"}, args...)...); code != 1 || !strings.Contains(errw, "not on a forgotten node") || len(writes(r)) != 0 {
		t.Errorf("not lost = %d %q, writes %v", code, errw, writes(r))
	}
	if code, _, errw = cli(t, "env", "restore", "--to", "h5"); code != 2 {
		t.Errorf("no --placement = %d %q", code, errw)
	}
}

func TestEnvCreatePlacementAndLs(t *testing.T) {
	r := fake(t, map[string]string{"/envs": `[{"slug":"dev","name":"dev","type":"static","node_id":"n0"}]`})
	if code, out, errw := cli(t, "env", "ls", "--stack", "shop"); code != 0 || !strings.Contains(out, "dev\tdev\tstatic") || strings.Contains(out, "home") {
		t.Errorf("ls = %d %q %q", code, out, errw)
	}
	if len(r.reqs) != 1 { // members cannot read /nodes: env ls asks for envs only
		t.Errorf("ls sent %v", r.reqs)
	}
	if code, _, errw := cli(t, "env", "create", "demo", "--stack", "shop", "--from-kind", "promote", "--placement", "eu-prod"); code != 0 {
		t.Fatalf("create = %d %q", code, errw)
	}
	w := writes(r)
	if len(w) != 1 || !strings.Contains(w[0], `"placement":"eu-prod"`) {
		t.Errorf("create writes = %v", w)
	}
}

func TestClusterStatus(t *testing.T) {
	good := `{"nodes":[{"name":"home","role":"controller","state":"online","tags":["ingress"],"envs":2}],"groups":[{"name":"internet","nodes":["home"]}],"links":[],"problems":[]}`
	fake(t, map[string]string{"/cluster": good})
	code, out, errw := cli(t, "cluster", "status")
	if code != 0 || !strings.Contains(out, "INTERNET") || !strings.Contains(out, "all 1 node online") {
		t.Errorf("good = %d %q %q", code, out, errw)
	}
	bad := `{"nodes":[{"name":"nas","role":"worker","state":"offline","tags":[],"envs":2}],"groups":[],"links":[],"problems":["nas is offline"]}`
	fake(t, map[string]string{"/cluster": bad})
	if code, out, _ = cli(t, "cluster", "status"); code != 1 || !strings.Contains(out, "1 problem: nas is offline") {
		t.Errorf("bad = %d %q", code, out)
	}
	if code, out, _ = cli(t, "--json", "cluster", "status"); code != 1 || !strings.Contains(out, `"problems"`) {
		t.Errorf("--json = %d %q", code, out)
	}
}

func TestAdminAudit(t *testing.T) {
	events := `[` +
		`{"at":"2026-10-12T14:02:11Z","action":"node.confirm","actor_id":"u1","target_kind":"node","target_id":"n1","ip":"","detail":"range"},` +
		`{"at":"2026-10-12T12:15:44Z","action":"login.bad","actor_id":"","target_kind":"user","target_id":"x","ip":"203.0.113.9","detail":""},` +
		`{"at":"2026-10-12T11:03:20Z","action":"cert.issue","actor_id":"","target_kind":"cert","target_id":"c1","ip":"","detail":""}]`
	r := fake(t, map[string]string{"/admin/security-log": events, "/admin/users": `[{"id":"u1","email":"admin@example.com"}]`, "/nodes": `[{"id":"n1","name":"vps-fsn1"}]`})
	code, out, errw := cli(t, "admin", "audit", "--since", "24h")
	if code != 0 || !strings.Contains(out, "admin@example.com\tnode.confirm\tvps-fsn1\tok") || !strings.Contains(out, "203.0.113.9\tlogin.bad\tx\tfailed") || !strings.Contains(out, "system\tcert.issue") {
		t.Fatalf("audit = %d %q %q", code, out, errw)
	}
	if !strings.Contains(strings.Join(r.reqs, "\n"), "GET /api/v1/admin/security-log?since=") {
		t.Errorf("since was not sent: %v", r.reqs)
	}
	if _, out, _ = cli(t, "admin", "audit", "--action", "cert.*"); strings.Contains(out, "node.confirm") || !strings.Contains(out, "cert.issue") {
		t.Errorf("--action = %q", out)
	}
	if _, out, _ = cli(t, "admin", "audit", "--outcome", "failed"); strings.Contains(out, "node.confirm") || !strings.Contains(out, "login.bad") {
		t.Errorf("--outcome = %q", out)
	}
	if _, out, _ = cli(t, "admin", "audit", "--actor", "admin@example.com"); strings.Contains(out, "login.bad") || !strings.Contains(out, "node.confirm") {
		t.Errorf("--actor = %q", out)
	}
	if _, out, _ = cli(t, "admin", "audit", "--target", "vps-fsn1"); strings.Contains(out, "cert.issue") || !strings.Contains(out, "node.confirm") {
		t.Errorf("--target = %q", out)
	}
	if code, _, errw = cli(t, "admin", "audit", "--since", "yesterday"); code != 2 {
		t.Errorf("bad --since = %d %q", code, errw)
	}
	if code, _, _ = cli(t, "admin", "audit", "--outcome", "maybe"); code != 2 {
		t.Errorf("bad --outcome = %d", code)
	}
	if code, out, _ = cli(t, "--json", "admin", "audit", "--outcome", "ok"); code != 0 || !strings.Contains(out, `"outcome": "ok"`) || strings.Contains(out, "login.bad") {
		t.Errorf("--json = %d %q", code, out)
	}
}

// The docker password is a short-lived registry token the panel mints, never
// the CLI's API key.
func TestRegistryLogin(t *testing.T) {
	var host, user, pw string
	old := dockerLogin
	dockerLogin = func(h, u, p string) error { host, user, pw = h, u, p; return nil }
	t.Cleanup(func() { dockerLogin = old })
	r := fake(t, map[string]string{"/registry/login": `{"token":"stackr-rl.t0k","expires_at":"2026-10-13T00:00:00Z"}`})
	code, out, errw := cli(t, "registry", "login", "--registry", "registry.example.com")
	if code != 0 || host != "registry.example.com" || user != "stackr" || pw != "stackr-rl.t0k" || pw == "k" || !strings.Contains(out, "Logged docker in to registry.example.com") {
		t.Errorf("login = %d %q %q host=%q user=%q pw=%q", code, out, errw, host, user, pw)
	}
	if w := writes(r); len(w) != 1 || !strings.HasPrefix(w[0], "POST /api/v1/registry/login") {
		t.Errorf("writes = %v", w)
	}
	useServer(t, "https://panel.example.com")
	fakeErr(t, 401, `{"error":"no"}`)
	if code, _, _ = cli(t, "registry", "login"); code == 0 || host != "registry.example.com" {
		t.Errorf("a refused mint reached docker: %d host=%q", code, host)
	}
}

// remove and backup are jobs: each sends one write that answers a job, then
// follows it.
func TestNodeRemoveAndBackupQueueJobs(t *testing.T) {
	r := fake(t, map[string]string{
		"/nodes/vps":              `{"name":"vps","env_list":[]}`,
		"/admin/nodes/vps":        `{"id":"j1"}`,
		"/admin/nodes/vps/backup": `{"id":"j2"}`,
	})
	cli(t, "-y", "node", "remove", "vps")
	cli(t, "-y", "node", "backup", "vps")
	want := []string{"DELETE /api/v1/admin/nodes/vps ", "POST /api/v1/admin/nodes/vps/backup "}
	if w := writes(r); len(w) != 2 || w[0] != want[0] || w[1] != want[1] {
		t.Errorf("writes = %q, want %q", w, want)
	}
	if code, _, _ := cli(t, "node", "backup", "vps"); code != 3 {
		t.Errorf("backup without -y = %d, want the confirm refusal (3)", code)
	}
}

// ingress set --default shows where the hosts point, then sends the typed
// name as confirm; without it and without a terminal nothing is sent.
func TestIngressSetDefaultShowsPlanAndSendsConfirm(t *testing.T) {
	plan := `{"node":"h4","addrs":["203.0.113.4"],"known":true,"hosts":[` +
		`{"host":"panel.example.com","panel":true,"points":["198.51.100.9"],"ok":false},` +
		`{"host":"app.example.com","panel":false,"points":["203.0.113.4"],"ok":true}]}`
	r := fake(t, map[string]string{"/admin/default-ingress/plan": plan, "/admin/default-ingress": `{"ingress":"h4","exit":""}`})
	if code, _, errw := cli(t, "ingress", "set", "h4", "--default"); code != 2 || !strings.Contains(errw, "--confirm h4") || len(writes(r)) != 0 {
		t.Errorf("no confirm = %d %q, writes %v", code, errw, writes(r))
	}
	code, out, errw := cli(t, "ingress", "set", "h4", "--default", "--confirm", "h4")
	if code != 0 {
		t.Fatalf("confirmed = %d %q %q", code, out, errw)
	}
	for _, s := range []string{"panel.example.com", "points to 198.51.100.9", "points here"} {
		if !strings.Contains(out, s) {
			t.Errorf("plan output lacks %q: %q", s, out)
		}
	}
	if w := writes(r); len(w) != 1 || w[0] != `PUT /api/v1/admin/default-ingress {"confirm":"h4","node":"h4"}` {
		t.Errorf("writes = %q", w)
	}
}

func TestGlobMatch(t *testing.T) {
	for pat, want := range map[string]bool{"cert.*": true, "cert.issue": true, "*.issue": true, "node.*": false, "c*e*e": true, "cert": false} {
		if got := globMatch(pat, "cert.issue"); got != want {
			t.Errorf("globMatch(%q) = %v", pat, got)
		}
	}
}

// job log reads a server job (the org route 404s on it) under /admin
// without --admin; a job neither has keeps the org's not-found.
func TestJobLogFindsServerJob(t *testing.T) {
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		reqs = append(reqs, req.URL.Path)
		if strings.HasPrefix(req.URL.Path, "/api/v1/orgs/") || strings.Contains(req.URL.Path, "/nope") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no job"}`)
			return
		}
		_, _ = io.WriteString(w, `{"job":{"id":"j1"},"log":"hello\n"}`)
	}))
	t.Cleanup(srv.Close)
	useServer(t, srv.URL)
	if code, out, errw := cli(t, "job", "log", "j1"); code != 0 || out != "hello\n" {
		t.Fatalf("log = %d %q %q", code, out, errw)
	}
	if last := reqs[len(reqs)-1]; last != "/api/v1/admin/jobs/j1/log" {
		t.Errorf("read %q, want the admin route", last)
	}
	if code, _, errw := cli(t, "job", "log", "nope"); code == 0 || !strings.Contains(errw, "no job") {
		t.Errorf("missing = %d %q", code, errw)
	}
}
