package setup_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/handler/setup"
)

// The Nodes step adds a node as Admin does: name and tags, the command, the
// machine calling in, Add; the list shows it joined. All of it answers
// into the step's panel while the wizard is unfinished.
func TestServerWizardAddNode(t *testing.T) {
	s := newSrvSite(t)
	ctx := context.Background()
	uid := s.env.User(t, "root@x.test", true)
	adm := s.env.Session(t, uid)
	nodes := setup.ServerBase + "/nodes"
	want := func(body string, ws ...string) {
		t.Helper()
		for _, w := range ws {
			if !strings.Contains(body, w) {
				t.Fatalf("no %q in\n%s", w, body)
			}
		}
	}

	page := s.do(adm, "GET", setup.ServerBase+"/ui/nodes", nil, false)
	want(page.Body.String(), `hx-get="`+nodes+`?add=1"`, "controller", `hx-trigger="every 5s"`)

	rec := s.do(adm, "GET", nodes+"?add=1", nil, true)
	want(rec.Body.String(), "Get join command", `hx-post="`+nodes+`/add"`, `hx-target="#setup-nodes"`)

	rec = s.do(adm, "POST", nodes+"/add", url.Values{"name": {"Bad Name"}}, true)
	want(rec.Body.String(), "Get join command", "lowercase")

	rec = s.do(adm, "POST", nodes+"/add", url.Values{"name": {"w5"}, "tag": {"builder"}}, true)
	want(rec.Body.String(), "Waiting for w5 to call in", nodes+"/add/poll?name=w5", nodes+"?skip=w5")
	if rec := s.do(adm, "GET", nodes+"/add/poll?name=w5", nil, true); rec.Code != http.StatusNoContent {
		t.Fatalf("poll before the call in = %d", rec.Code)
	}

	// the box runs the command
	by := &service.Principal{User: service.User{ID: uid}, Access: authz.User{ID: uid, Active: true, Admin: true}}
	tok, err := s.env.Orch.CreateJoinToken(ctx, by, service.JoinTokenSpec{NodeName: "w6"})
	if err != nil {
		t.Fatal(err)
	}
	key, _ := clusterca.NewKey()
	csr, _ := clusterca.NewCSR(key)
	if _, err := s.env.Orch.RequestJoin(ctx, service.JoinMeta{HTTPS: true}, service.JoinRequest{
		Token: tok.Token, CSR: string(csr), WGPublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}); err != nil {
		t.Fatal(err)
	}
	rec = s.do(adm, "GET", nodes+"/add/poll?name=w6", nil, true)
	want(rec.Body.String(), "Add w6", nodes+"/pending/", `hx-target="#setup-nodes"`)
	// the list offers it too, unless open steps wait for it
	if body := s.do(adm, "GET", nodes, nil, true).Body.String(); !strings.Contains(body, "called in, waiting for you") {
		t.Errorf("the list does not offer the pending machine:\n%s", body)
	}
	if body := s.do(adm, "GET", nodes+"?skip=w6", nil, true).Body.String(); strings.Contains(body, "called in, waiting for you") {
		t.Errorf("the list offers the machine the open steps wait for:\n%s", body)
	}

	ps, err := s.env.Orch.PendingNodes(ctx)
	if err != nil || len(ps) != 1 {
		t.Fatalf("pending = %+v, %v", ps, err)
	}
	rec = s.do(adm, "POST", nodes+"/pending/"+ps[0].ID+"/confirm", nil, true)
	want(rec.Body.String(), "w6", "Joined, not seen yet")
}
