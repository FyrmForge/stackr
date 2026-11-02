package dialog

import (
	"context"
	"strings"
	"testing"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// The form shows the fields of its source; the confirms build (Delete and
// Rollback are type-the-name, so they must say what is kept).
func TestCreateTileBySource(t *testing.T) {
	for src, want := range map[string]string{
		"image":    `name="image_ref"`,
		"service":  `name="git_url"`,
		"cron":     `name="schedule"`,
		"function": `name="trigger"`,
		"managed":  `name="engine"`,
		"slice":    `placeholder="infra:${{ env.name }}:pg-db"`,
	} {
		var b strings.Builder
		v := CreateTileView{
			Action: "/x",
			Switch: "/x",
			Source: src,
			Hosts:  "github.com",
			Errors: map[string]string{"name": "taken"},
		}
		if err := CreateTile(v).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), want) || !strings.Contains(b.String(), "taken") ||
			!strings.Contains(b.String(), `value="`+src+`" checked`) {
			t.Errorf("%s form:\n%s", src, b.String())
		}
	}
	// the dump field is postgres only; the engine select re-renders the form
	for engine, dump := range map[string]bool{"": true, "postgres": true, "s3": false} {
		var b strings.Builder
		if err := CreateTile(CreateTileView{Action: "/x", Switch: "/sw", Source: "managed", Engine: engine}).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		if got := b.String(); strings.Contains(got, `name="dump"`) != dump || !strings.Contains(got, `name="engine" class="input" hx-get="/sw"`) {
			t.Errorf("managed %q: dump field %v, want %v:\n%s", engine, strings.Contains(got, `name="dump"`), dump, got)
		}
	}
	for _, v := range []c.ConfirmView{
		Stop("api", "/s", "#d"),
		Delete("api", "/d", "#d"),
		Rollback("dev", "release 3", "/rb", "#d"),
	} {
		if err := c.Confirm(v).Render(context.Background(), &strings.Builder{}); err != nil {
			t.Fatal(err)
		}
	}
}
