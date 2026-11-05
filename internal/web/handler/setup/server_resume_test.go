package setup_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/handler/setup"
)

// The by-hand branch remembers its screen: /-/setup/server and /ui resume
// there after a reload, and finishing clears it.
func TestServerWizardResume(t *testing.T) {
	s := newSrvSite(t)
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	s.do(adm, "POST", setup.ServerBase+"/mode", url.Values{"mode": {"ui"}}, true)
	if rec := s.do(adm, "GET", setup.ServerBase+"/ui/backups", nil, false); rec.Code != http.StatusOK {
		t.Fatalf("GET backups = %d", rec.Code)
	}
	for _, p := range []string{setup.ServerBase, setup.ServerBase + "/ui"} {
		if got := s.do(adm, "GET", p, nil, false).Header().Get("Location"); got != setup.ServerBase+"/ui/backups" {
			t.Errorf("GET %s = %q, want the backups step", p, got)
		}
	}
	s.do(adm, "POST", setup.ServerBase+"/done", url.Values{}, true)
	if st, _ := s.env.Orch.ServerSetupState(context.Background()); st.Step != "" {
		t.Errorf("step after finish = %q, want none", st.Step)
	}
}

// The file plan shows a missing server secret as a field to fill, not as a
// note to do it after finishing; a blank save is refused.
func TestServerPlanGapForm(t *testing.T) {
	s := newSrvSite(t)
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	err := s.env.Store.ServerPlans.Create(context.Background(), service.ServerPlan{
		ID: uuid.NewString(), Status: "pending", Source: "repo", Ticked: []string{}, CreatedAt: time.Now(),
		Plan: `{"gaps":[{"kind":"secret","scope":"server","key":"db.password"}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := s.do(adm, "GET", setup.ServerBase+"/config/plan", nil, false).Body.String()
	for _, w := range []string{`name="secret.db.password"`, `type="password"`, `hx-post="/-/setup/server/config/gaps/secret"`} {
		if !strings.Contains(body, w) {
			t.Errorf("plan lacks %q", w)
		}
	}
	if strings.Contains(body, "finish setup") {
		t.Error("plan still says to fill the gap after finish")
	}
	rec := s.do(adm, "POST", setup.ServerBase+"/config/gaps/secret", url.Values{"secret.db.password": {""}}, true)
	if rec.Header().Get("HX-Redirect") != setup.ServerBase+"/config/plan" {
		t.Errorf("blank save = %d %v", rec.Code, rec.Header())
	}
}
