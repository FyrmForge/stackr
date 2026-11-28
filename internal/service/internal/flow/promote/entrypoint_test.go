package promote

import (
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const epHead = "version: 1\nstack: shop\nladder: [dev]\nhead: main\nbase:\n  tiles:\n    api:\n      image: nginx:1\n      port: 80\n"

func TestParseEntrypoint(t *testing.T) {
	for src, want := range map[string]string{
		"      entrypoint: /bin/app --x\n":                       "/bin/app --x",
		"      entrypoint: [/bin/app, --x]\n":                    "/bin/app --x",
		"      entrypoint:\n        - /bin/app\n        - a b\n": "/bin/app 'a b'",
	} {
		r, err := Load([]byte(epHead+src), nil, "acme")
		must(t, err)
		got := string(firstEnv(r).Tiles["api"].Entrypoint)
		if got != want {
			t.Errorf("%q: got %q, want %q", src, got, want)
		}
	}
	r, _ := Load([]byte(epHead+"      entrypoint: [sh, 'a b', \"it's\"]\n"), nil, "acme")
	argv, err := deploy.SplitCommand(string(firstEnv(r).Tiles["api"].Entrypoint))
	must(t, err)
	if strings.Join(argv, "|") != "sh|a b|it's" {
		t.Errorf("argv = %q", argv)
	}
	for _, src := range []string{
		"      entrypoint: ${{ params.a.b }}\n",
		"      entrypoint: [\"${{ params.a.b }}\"]\n",
		"      entrypoint: {a: b}\n",
		"      entrypoint: \"a 'b\"\n",
	} {
		if _, err := Load([]byte(epHead+src), nil, "acme"); err == nil {
			t.Errorf("%q: loaded", src)
		}
	}
}

// The plan row carries the file's entrypoint, and export writes the stored
// one back as the string form. (That the plan shows a change and apply stores
// it rides on leaf/tile diff.go.)
func TestEntrypointRowAndExport(t *testing.T) {
	w := setup(t)
	r, err := Load([]byte(epHead+"      entrypoint: [/bin/app, 'a b']\n"), nil, "acme")
	must(t, err)
	row := toRow("api", firstEnv(r).Tiles["api"], w.st, w.dev)
	if row.Entrypoint != "/bin/app 'a b'" {
		t.Errorf("row entrypoint = %q", row.Entrypoint)
	}
	api := tileRow(t, w, "api", row.Entrypoint)
	out, _, err := w.f.Export(ctx, w.st.ID, w.dev.ID)
	must(t, err)
	if !strings.Contains(string(out), "entrypoint: /bin/app 'a b'") {
		t.Errorf("export (%s):\n%s", api.ID, out)
	}
	w.files["c3"] = string(out)
	rel := w.release(t, "c3")
	p, err := w.f.Plan(ctx, w.dev.ID, rel.ID, io.Discard)
	must(t, err)
	if len(p.Changes) != 0 {
		t.Errorf("plan of the export = %s", kinds(p))
	}
}

func tileRow(t *testing.T, w *world, name, ep string) store.Tile {
	t.Helper()
	tl, err := w.f.D.Tiles.Create(ctx, store.Tile{
		StackID:       w.st.ID,
		EnvironmentID: w.dev.ID,
		Name:          name,
		Kind:          tile.Image,
		ImageRef:      "nginx:1",
		ContainerPort: 80,
		Entrypoint:    ep,
	})
	must(t, err)
	return tl
}

// A change to the entrypoint alone shows a plan row, apply writes it, and the
// same file applied to the next rung (promote) carries it too.
func TestEntrypointOnlyChangePlansAppliesPromotes(t *testing.T) {
	w := setup(t)
	file := func(ep string) string {
		return "version: 1\nstack: shop\nladder: [dev, prd]\nhead: main\nbase:\n  tiles:\n    api:\n      image: nginx:1\n      port: 80\n" + ep
	}
	w.files["c1"] = file("")
	_, err := w.f.Apply(ctx, w.dev.ID, w.release(t, "c1").ID, io.Discard, nil)
	must(t, err)
	w.running(w.tileIn(t, w.dev, "api"))

	w.files["c2"] = file("      entrypoint: [/bin/app, 'a b']\n")
	r2 := w.release(t, "c2")
	p, err := w.f.Plan(ctx, w.dev.ID, r2.ID, io.Discard)
	must(t, err)
	if got := kinds(p); !strings.Contains(got, "entrypoint") {
		t.Fatalf("plan = %q, want an entrypoint row", got)
	}
	_, err = w.f.Apply(ctx, w.dev.ID, r2.ID, io.Discard, nil)
	must(t, err)
	if got := w.tileIn(t, w.dev, "api").Entrypoint; got != "/bin/app 'a b'" {
		t.Errorf("dev entrypoint = %q", got)
	}

	_, err = w.f.Apply(ctx, w.prd.ID, r2.ID, io.Discard, nil)
	must(t, err)
	if got := w.tileIn(t, w.prd, "api").Entrypoint; got != "/bin/app 'a b'" {
		t.Errorf("prd entrypoint = %q", got)
	}
}
