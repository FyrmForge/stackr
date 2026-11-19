package outbound

import (
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
)

func TestDrawer(t *testing.T) {
	var b strings.Builder
	v := View{Live: "/a/s/e/-/internet", Rows: []Row{
		{Name: "api", Live: true, Bytes: 3 << 20},
		{Name: "worker", LastSeen: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC), Bytes: 512},
	}}
	if err := body(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"api", "now", "3.0 MB", "worker", "2026-11-19T10:00:00Z", "512 B", `hx-get="/a/s/e/-/internet"`} {
		if !strings.Contains(b.String(), w) {
			t.Errorf("lacks %q", w)
		}
	}
	uitest.NoPanelInCard(t, "<section>"+b.String()+"</section>")
	b.Reset()
	_ = body(View{}).Render(t.Context(), &b)
	if !strings.Contains(b.String(), "No tile has reached out yet.") {
		t.Error("empty state missing")
	}
}
