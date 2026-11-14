package admin

import (
	"net/url"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

func TestRecipeFromForm(t *testing.T) {
	v, err := recipeFromForm(url.Values{
		"name":     {" pg_cron "},
		"majors":   {"16, 17"},
		"packages": {"postgresql-17-cron\n\n  postgresql-16-cron "},
		"sha256":   {"17-amd64=ab\n16-arm64 = cd"},
		"share":    {"only"},
		"org":      {"o1", "o2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "pg_cron" || !slices.Equal(v.Majors, []int{16, 17}) || len(v.Packages) != 2 ||
		v.SHA256["17-amd64"] != "ab" || v.SHA256["16-arm64"] != "cd" || !slices.Equal(v.Share, []string{"o1", "o2"}) {
		t.Errorf("parsed = %+v", v)
	}
	// orgs posted without "only" open nothing
	if v, _ := recipeFromForm(url.Values{"share": {"server"}, "org": {"o1"}}); len(v.Share) != 0 {
		t.Errorf("share = %v", v.Share)
	}
	if v, _ := recipeFromForm(url.Values{"share": {"all"}}); !slices.Equal(v.Share, []string{"all"}) {
		t.Errorf("share = %v", v.Share)
	}
	for field, f := range map[string]url.Values{
		"majors": {"majors": {"17, x"}},
		"sha256": {"sha256": {"no equals sign"}},
	} {
		_, err := recipeFromForm(f)
		if bad, ok := errs.IsInvalid(err); !ok || bad.Field != field {
			t.Errorf("%s: err = %v", field, err)
		}
	}
}
