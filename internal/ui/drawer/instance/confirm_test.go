package instance

import (
	"context"
	"io"
	"strings"
	"testing"
)

func renderTempl(t *testing.T, c interface {
	Render(context.Context, io.Writer) error
}) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Risky postgres actions ask first; the version choices say which way.
func TestRiskyActionsConfirm(t *testing.T) {
	v := View{Base: "/o/s/e/-/instances/pg", Engine: "postgres"}
	sw := renderTempl(t, failover(v, FailoverView{Switch: []string{"n2"}}))
	for name, tc := range map[string]struct {
		got  string
		want []string
	}{
		"pitr on":    {renderPITR(t, PITRView{Edit: true}), []string{"Restarts the database once."}},
		"pitr days":  {renderPITR(t, PITRView{Edit: true, On: true, Days: 7}), []string{"Change the retention?"}},
		"switchover": {sw, []string{"Clients reconnect once.", `hx-post="/o/s/e/-/instances/pg/switchover"`}},
		"promote":    {renderTempl(t, failover(v, FailoverView{Promote: []string{"n2"}})), []string{"Promote the standby?"}},
		"abort":      {renderTempl(t, failover(v, FailoverView{InFlight: "x", CanAbort: true})), []string{"Abort the switchover?"}},
		"major":      {renderTempl(t, majorChange(v, VersionView{Image: "postgres:17.5", Majors: []int{16, 18}})), []string{"Change the postgres version?", "Downgrade to 16", "Upgrade to 18"}},
	} {
		if !strings.Contains(tc.got, "<confirm-dialog") {
			t.Errorf("%s has no confirm", name)
		}
		for _, w := range tc.want {
			if !strings.Contains(tc.got, w) {
				t.Errorf("%s lacks %q", name, w)
			}
		}
	}
	if strings.Contains(sw, "fence") {
		t.Error("fence is not a failover action here")
	}
	// the confirm posts, never the form: Enter must not submit past it, and an
	// open dialog must survive a live swap.
	for name, got := range map[string]string{"pitr": renderPITR(t, PITRView{Edit: true}), "switch": sw} {
		if strings.Contains(got, "<form hx-post") || strings.Contains(got, "<form novalidate hx-post") || strings.Contains(got, "hx-preserve hx-post") {
			t.Errorf("%s form posts on its own", name)
		}
	}
	if got := renderTempl(t, failover(v, FailoverView{InFlight: "x", CanAbort: true})); !strings.Contains(got, "hx-preserve") {
		t.Error("abort confirm is not preserved across live swaps")
	}
}
