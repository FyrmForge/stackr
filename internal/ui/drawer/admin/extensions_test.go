package admin

import (
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// Built-ins show a switch and no edit or delete; an own recipe shows its edit
// form and delete; each form posts to its own action.
func TestExtensionsSection(t *testing.T) {
	share := c.ShareView{ModeName: "share", OrgName: "org", Mode: c.ShareAll}
	edit := RecipeForm{ID: "ext-mine", Action: "/-/admin/extensions/mine", Share: share}
	v := ExtensionsView{
		Builtins: []RecipeRow{{Name: "pgvector", Info: "postgres 14 to 18", Enabled: true, Toggle: "/-/admin/extensions/pgvector/enabled?on=false"}},
		Own:      []RecipeRow{{Name: "mine", Edit: &edit, Delete: c.ConfirmView{Button: "Delete", Title: "Delete mine?", Action: "/-/admin/extensions/mine/delete", Target: Target}}},
		Add:      RecipeForm{ID: "ext-add", Action: "/-/admin/extensions", Share: share},
	}
	var b strings.Builder
	if err := Extensions(v).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{"pgvector", "Switch off", `hx-post="/-/admin/extensions/mine"`, `hx-post="/-/admin/extensions"`, "/-/admin/extensions/mine/delete", `id="ext-add-name"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(got, "extensions/pgvector/delete") || strings.Contains(got, `id="ext-pgvector-`) {
		t.Error("a built-in offers edit or delete")
	}
}
