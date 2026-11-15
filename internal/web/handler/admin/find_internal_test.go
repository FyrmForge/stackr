package admin

import (
	"strings"
	"testing"

	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
)

func plain(ps []ui.FindPart) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.Text)
	}
	return b.String()
}

func TestMatchFind(t *testing.T) {
	es := []findEntry{
		{Key: "update_check", Label: "Check for updates", Help: "How often it looks.", Section: "update", SectionLabel: "Update"},
		{Key: "workers", Label: "Workers", Help: "Update jobs run at once.", Section: "jobs", SectionLabel: "Jobs"},
		{Key: "update_window", Label: "Window", Help: "When.", Section: "update", SectionLabel: "Update"},
		{Label: "Update", Help: "This server's version.", Section: "update"},
	}
	cases := []struct {
		name, q string
		want    []string // hrefs in order
	}{
		{"empty", "  ", nil},
		{"label, case-insensitive", "CHECK", []string{"/-/admin/update?find=update_check"}},
		{"key with underscore", "update_window", []string{"/-/admin/update?find=update_window"}},
		{"key with space", "update window", []string{"/-/admin/update?find=update_window"}},
		{"help", "run at once", []string{"/-/admin/jobs?find=workers"}},
		{"none", "zzz", nil},
		{"order: label, key, help, section", "update", []string{
			"/-/admin/update?find=update_check",
			"/-/admin/update?find=update_window",
			"/-/admin/jobs?find=workers",
			"/-/admin/update",
		}},
	}
	for _, c := range cases {
		v := matchFind(c.q, es)
		var got []string
		for _, h := range v.Hits {
			got = append(got, h.Href)
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if v := matchFind("", es); v.Asked {
		t.Error("an empty query was asked")
	}
	if v := matchFind("check", es); plain(v.Hits[0].Label) != "Check for updates" || !v.Hits[0].Label[0].Hit || v.Hits[0].Label[0].Text != "Check" {
		t.Errorf("label parts = %+v", v.Hits[0].Label)
	}
	// special characters stay text; templ escapes them
	hs := matchFind("<b>", []findEntry{{Key: "k", Label: "A <b> & c", Section: "jobs"}})
	if len(hs.Hits) != 1 || hs.Hits[0].Label[1].Text != "<b>" || !hs.Hits[0].Label[1].Hit {
		t.Errorf("special query = %+v", hs)
	}
}

func TestMatchFindCap(t *testing.T) {
	var es []findEntry
	for i := 0; i < 11; i++ {
		es = append(es, findEntry{Key: "k", Label: "Same", Section: "jobs"})
	}
	if v := matchFind("same", es); len(v.Hits) != findCap || v.Total != 11 {
		t.Errorf("hits %d total %d", len(v.Hits), v.Total)
	}
}
