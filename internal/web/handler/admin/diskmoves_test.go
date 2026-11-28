package admin_test

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The controller's card offers Move logs and metrics (and no panel database
// row on a panel that is not installed); the plan refuses in its dialog; a
// running move turns the button off, and the live region and job slot follow
// it.
func TestMovesRowsOnTheCard(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	card := section(t, s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String())
	if strings.Contains(card, "Move logs and metrics") {
		t.Errorf("no disk, yet moves are offered\n%s", card)
	}
	addedDisk(t, s, root, id, "/data")
	body := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	card = section(t, body)
	base := "/-/admin/nodes/" + id + "/disk/move-observe"
	has(t, "rows", card, ">Move logs and metrics onto this disk</button>", `hx-post="`+base+`/plan"`, `id="observe-dlg"`)
	if strings.Contains(card, "Move the panel database") {
		t.Errorf("the panel database row on a panel that is not installed\n%s", card)
	}
	noBox(t, "rows", card)
	has(t, "live", body, `id="node-live"`, `hx-trigger="every 3s"`, `id="job-slot"`)

	plan := s.As(t, root, "POST", base+"/plan", url.Values{}).Body.String()
	has(t, "plan", plan, "Move logs and metrics onto this disk", `role="alert"`, "Logs and metrics stop while the data copies")
	pplan := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/move-paneldb/plan", url.Values{"back": {"1"}}).Body.String()
	has(t, "panel plan", pplan, "Move the panel database back to the system disk", `role="alert"`, "This panel does not run as an installed container.")
	rec := s.As(t, root, "POST", base, url.Values{})
	if rec.Header().Get("HX-Retarget") != "#observe-dlg" {
		t.Errorf("a refused press: %d, HX-Retarget %q", rec.Code, rec.Header().Get("HX-Retarget"))
	}

	seedJob(t, s, "j-obs", "observe-move", "running", []string{"todisk:" + id}, `{"node_id":"`+id+`"}`)
	card = section(t, s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String())
	if !regexp.MustCompile(`<button[^>]*\sdisabled(\s[^>]*)?>Move logs and metrics onto this disk</button>`).MatchString(card) {
		t.Errorf("the button is on while the move runs\n%s", card)
	}
}
