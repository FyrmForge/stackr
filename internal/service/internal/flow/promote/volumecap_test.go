package promote

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
)

const capHead = "version: 1\nstack: shop\nladder: [dev]\nhead: main\nvolumes:\n"

func TestParseVolumeMaxSize(t *testing.T) {
	for src, ok := range map[string]bool{
		"  data: {max_size_mb: 100}\n":  true,
		"  data: {max_size_mb: 0}\n":    true,
		"  data: {max_size_mb: null}\n": true,
		"  data: {}\n":                  true,
		"  data: {max_size_mb: -1}\n":   false,
	} {
		_, err := Load([]byte(capHead+src), nil, "acme")
		if (err == nil) != ok {
			t.Errorf("%q: err = %v", src, err)
		}
	}
}

// The stack file's max_size_mb is planned and applied through SetVolumeCap;
// a file without the key leaves the cap alone, 0 clears it, and export writes
// it back.
func TestVolumeCapFromFile(t *testing.T) {
	w := setup(t)
	var calls []string
	w.f.SetVolumeCap = func(c context.Context, id string, mb *int64) error {
		v, err := w.f.D.Volumes.Get(c, id)
		must(t, err)
		v.MaxSizeMB = mb
		must(t, w.s.Volumes.Update(c, v))
		if mb == nil {
			calls = append(calls, "none")
		} else {
			calls = append(calls, strconv.FormatInt(*mb, 10))
		}
		return nil
	}
	apply := func(commit, body string) *Plan {
		t.Helper()
		w.files[commit] = capHead + body
		r := w.release(t, commit)
		p, err := w.f.Apply(ctx, w.dev.ID, r.ID, io.Discard, nil)
		must(t, err)
		return p
	}
	capNow := func() string {
		v, err := w.f.D.Volumes.BySlug(ctx, volume.Scope{Kind: "env", ID: w.dev.ID}, "data")
		must(t, err)
		if v.MaxSizeMB == nil {
			return "none"
		}
		return strconv.FormatInt(*v.MaxSizeMB, 10)
	}

	apply("c1", "  data: {max_size_mb: 100}\n")
	if capNow() != "100" {
		t.Errorf("cap after the first apply = %s", capNow())
	}
	p := apply("c2", "  data: {max_size_mb: 200}\n")
	if !strings.Contains(kinds(p), "volume cap:data") || capNow() != "200" {
		t.Errorf("plan %s, cap %s", kinds(p), capNow())
	}

	// export writes the cap back, and planning it is clean
	out, _, err := w.f.Export(ctx, w.st.ID, w.dev.ID)
	must(t, err)
	if !strings.Contains(string(out), "max_size_mb: 200") {
		t.Errorf("export lacks the cap:\n%s", out)
	}
	w.files["c3"] = string(out)
	r := w.release(t, "c3")
	p2, err := w.f.Plan(ctx, w.dev.ID, r.ID, io.Discard)
	must(t, err)
	if strings.Contains(kinds(p2), "volume") {
		t.Errorf("plan of the export = %s", kinds(p2))
	}

	n := len(calls)
	p = apply("c4", "  data: {}\n")
	if strings.Contains(kinds(p), "volume") || len(calls) != n || capNow() != "200" {
		t.Errorf("absent key: plan %s, calls %v, cap %s", kinds(p), calls, capNow())
	}
	apply("c5", "  data: {max_size_mb: 0}\n")
	if capNow() != "none" {
		t.Errorf("cap after 0 = %s", capNow())
	}
}
