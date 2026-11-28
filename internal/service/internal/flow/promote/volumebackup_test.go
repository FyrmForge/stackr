package promote

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
)

func TestParseVolumeBackup(t *testing.T) {
	for src, ok := range map[string]bool{
		"  data:\n    backup: none\n":                                      true,
		"  data:\n    backup:\n      schedule: '@daily'\n      keep: 7\n":  true,
		"  data:\n    backup:\n      schedule: '0 3 * * *'\n":              true,
		"  data:\n    backup: daily\n":                                     false,
		"  data:\n    backup: true\n":                                      false,
		"  data:\n    backup:\n      keep: 7\n":                            false,
		"  data:\n    backup:\n      schedule: 'nope'\n":                   false,
		"  data:\n    backup:\n      schedule: '@every 1h'\n":              false, // the panel refuses it too
		"  data:\n    backup:\n      schedule: '0  3 * *  *'\n":            true,
		"  data:\n    backup:\n      schedule: '@daily'\n      keep: -1\n": false,
		"  data:\n    backup:\n      schedule: '@daily'\n      dest: x\n":  false,
	} {
		_, err := Load([]byte(capHead+src), nil, "acme")
		if (err == nil) != ok {
			t.Errorf("%q: err = %v", src, err)
		}
	}
}

// The schedule is kept with its whitespace collapsed, so a plan that compares
// it with the stored one sees no change.
func TestVolumeBackupCronNormalised(t *testing.T) {
	var b VolumeBackup
	if err := yaml.Unmarshal([]byte("schedule: '0  3 *\t* *'\nkeep: 2\n"), &b); err != nil || b.Schedule != "0 3 * * *" {
		t.Fatalf("got %+v, %v", b, err)
	}
}

// fake schedule store behind the Flow's two backup funcs
type fakeBackup struct {
	cron  string
	keep  int
	n     int
	calls []string
}

func (b *fakeBackup) wire(w *world) {
	w.f.VolumeBackup = func(context.Context, string) (string, int, int, error) { return b.cron, b.keep, b.n, nil }
	w.f.SetVolumeBackup = func(_ context.Context, _ string, cron string, keep int) error {
		b.calls = append(b.calls, cron+"/"+strconv.Itoa(keep))
		b.cron, b.keep, b.n = cron, keep, 1
		if cron == "" {
			b.n = 0
		}
		return nil
	}
}

func TestVolumeBackupFromFile(t *testing.T) {
	w := setup(t)
	fb := &fakeBackup{}
	fb.wire(w)
	apply := func(commit, body string) *Plan {
		t.Helper()
		w.files[commit] = capHead + body
		r := w.release(t, commit)
		p, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
		must(t, err)
		return p
	}
	plan := func(commit, body string) *Plan {
		t.Helper()
		w.files[commit] = capHead + body
		r := w.release(t, commit)
		p, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
		must(t, err)
		return p
	}

	// new volume: declared, then the schedule is set
	p := apply("c1", "  data:\n    backup:\n      schedule: '@daily'\n      keep: 7\n")
	if !strings.Contains(kinds(p), "volume:") || strings.Join(fb.calls, ",") != "@daily/7" {
		t.Errorf("new volume: plan %s calls %v", kinds(p), fb.calls)
	}
	// same schedule: no row, no call
	n := len(fb.calls)
	p = apply("c2", "  data:\n    backup:\n      schedule: '@daily'\n      keep: 7\n")
	if strings.Contains(kinds(p), "volume") || len(fb.calls) != n {
		t.Errorf("same schedule: plan %s calls %v", kinds(p), fb.calls)
	}
	// absent key: nothing, even though a schedule exists
	p = apply("c3", "  data: {}\n")
	if strings.Contains(kinds(p), "volume") || len(fb.calls) != n {
		t.Errorf("absent: plan %s calls %v", kinds(p), fb.calls)
	}
	// a different keep is a row
	p = apply("c4", "  data:\n    backup:\n      schedule: '@daily'\n      keep: 3\n")
	if !strings.Contains(kinds(p), "volume backup:data") || fb.calls[len(fb.calls)-1] != "@daily/3" {
		t.Errorf("keep: plan %s calls %v", kinds(p), fb.calls)
	}
	// export, then plan it: no change
	out, _, err := w.f.Export(ctx, w.st.ID, w.dev.ID)
	must(t, err)
	if !strings.Contains(string(out), "schedule: '@daily'") && !strings.Contains(string(out), "schedule: \"@daily\"") && !strings.Contains(string(out), "schedule: '@daily'") {
		t.Errorf("export lacks the schedule:\n%s", out)
	}
	w.files["c5"] = string(out)
	r := w.release(t, "c5")
	p2, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p2), "volume") {
		t.Errorf("plan of the export = %s", kinds(p2))
	}
	// none removes
	p = apply("c6", "  data:\n    backup: none\n")
	if !strings.Contains(kinds(p), "volume backup:data") || fb.calls[len(fb.calls)-1] != "/0" {
		t.Errorf("none: plan %s calls %v", kinds(p), fb.calls)
	}
	if p = plan("c7", "  data:\n    backup: none\n"); strings.Contains(kinds(p), "volume") {
		t.Errorf("none again: %s", kinds(p))
	}
	// zero schedules: export writes no backup key
	out, _, err = w.f.Export(ctx, w.st.ID, w.dev.ID)
	must(t, err)
	if strings.Contains(string(out), "backup") {
		t.Errorf("export of no schedule:\n%s", out)
	}
	// several: left out with a warning, and the plan of the export leaves them
	fb.n = 2
	out, warns, err := w.f.Export(ctx, w.st.ID, w.dev.ID)
	must(t, err)
	if strings.Contains(string(out), "backup") || !strings.Contains(strings.Join(warns, "\n"), "several backup schedules") {
		t.Errorf("several: warns %v\n%s", warns, out)
	}
	n = len(fb.calls)
	w.files["c8"] = string(out)
	r = w.release(t, "c8")
	if _, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil); err != nil || len(fb.calls) != n {
		t.Errorf("apply of the several export: %v calls %v", err, fb.calls)
	}
	// cap and backup together are two rows
	fb.cron, fb.keep, fb.n = "@daily", 7, 1
	p = plan("c9", "  data:\n    max_size_mb: 50\n    backup: none\n")
	if !strings.Contains(kinds(p), "volume cap:data") || !strings.Contains(kinds(p), "volume backup:data") {
		t.Errorf("both: %s", kinds(p))
	}
	_ = volume.Scope{}
}
