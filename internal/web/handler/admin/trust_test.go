package admin_test

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/installspec"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Keys carries the cluster root for an admin: the root,
// the buttons, and break glass saying plainly what stops.
func TestAdminClusterTrust(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))

	rec := s.As(t, root, "GET", "/-/admin/keys", nil)
	body := rec.Body.String()
	for _, want := range []string{
		"Cluster root", "Rotate root", "Revoke controller certificate", "Break glass",
		"None running.", "Tunnels between nodes stop until each node rejoins.",
		"Apps keep running where they are.", "Deploys and traffic between nodes stop.",
	} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("keys = %d, no %q in\n%s", rec.Code, want, body)
		}
	}
	if strings.Contains(body, "Finish rollover now") || strings.Contains(body, "—") {
		t.Errorf("finish button with no rollover, or an em dash")
	}

	// break glass needs the typed words on the server too
	rec = s.As(t, root, "POST", "/-/admin/nodes/trust/break-glass", url.Values{"confirm": {"yes"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "break glass") {
		t.Errorf("wrong word = %d\n%s", rec.Code, rec.Body)
	}

	// an org owner reaches none of it
	for _, p := range []string{"rotate-root", "finish-rollover", "revoke-controller-cert", "break-glass"} {
		if rec := s.Do(t, "POST", "/-/admin/nodes/trust/"+p, url.Values{"confirm": {"break glass"}}); rec.Code != http.StatusForbidden {
			t.Errorf("owner POST %s = %d, want 403", p, rec.Code)
		}
	}
	if rec := s.Do(t, "POST", "/-/admin/nodes/w1/trust-rejoin", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner trust-rejoin = %d, want 403", rec.Code)
	}
}

// After break glass the section lists each node not rejoined with a rejoin
// action, and shows the command once it is made.
func TestTrustViewNotRejoined(t *testing.T) {
	v := ui.KeysView{Trust: ui.TrustView{
		Fingerprint: "ab12", Created: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Expires: "Jan 2, 2036", RollingOver: true,
		Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), OnOldRoot: []string{"h3"},
		Finish: comp.ConfirmView{Button: "Finish rollover now", Title: "t"},
		NotRejoined: []ui.NotRejoinedRow{
			{Name: "vm106", Rejoin: "/-/admin/nodes/n2/trust-rejoin"},
			{Name: "h3", Rejoin: "/-/admin/nodes/n3/trust-rejoin", Command: "stackr-install join --token T", Expires: "in 30 minutes"},
		},
	}}
	var b bytes.Buffer
	if err := ui.Keys(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Still on the old root: h3.", "Finish rollover now", "Not rejoined", "vm106",
		`hx-post="/-/admin/nodes/n2/trust-rejoin"`, "Rejoin command", "stackr-install join --token T", "Expires in 30 minutes.",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q in\n%s", want, b.String())
		}
	}
}

// An admin runs every trust button end to end: rotate (a worker on the old
// root keeps it running), the forced finish, revoke the controller cert,
// break glass, then a rejoin command for the worker left out.
func TestAdminClusterTrustVerbs(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	b, err := os.ReadFile(filepath.Join(s.Config.DataDir, installspec.ClusterCACertFile))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := clusterca.ParseCertPEM(b)
	if err != nil {
		t.Fatal(err)
	}
	w1 := s.Worker(t, "w1", clusterca.Fingerprint(ca))

	for _, step := range []struct {
		path string
		form url.Values
		say  string // in the answer
		tab  string // on Keys after
	}{
		{"trust/rotate-root", nil, "Rotating the root.", "Still on the old root: w1."},
		{"trust/finish-rollover", nil, "Rollover finished.", "None running."},
		{"trust/revoke-controller-cert", nil, "Replacing the controller certificate.", "None running."},
		{"trust/break-glass", url.Values{"confirm": {"break glass"}}, "Break glass done.", "Not rejoined"},
		{w1 + "/trust-rejoin", nil, "New join command below.", "Not rejoined"},
	} {
		rec := s.As(t, root, "POST", "/-/admin/nodes/"+step.path, step.form)
		// The step before may still run detached for a moment: refused, retried.
		for i := 0; i < 100 && strings.Contains(rec.Body.String(), "Try again in a few minutes"); i++ {
			time.Sleep(20 * time.Millisecond)
			rec = s.As(t, root, "POST", "/-/admin/nodes/"+step.path, step.form)
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), step.say) {
			t.Fatalf("POST %s = %d, no %q in\n%s", step.path, rec.Code, step.say, rec.Body)
		}
		if step.path == w1+"/trust-rejoin" && !strings.Contains(rec.Body.String(), "stackr-install join") {
			t.Fatalf("no rejoin command in\n%s", rec.Body)
		}
		if rec := s.As(t, root, "GET", "/-/admin/keys", nil); !strings.Contains(rec.Body.String(), step.tab) {
			t.Fatalf("after %s: no %q on Keys", step.path, step.tab)
		}
	}
}
