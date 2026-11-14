package backup

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"testing"

	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
)

// imageBase is a Base subject whose image is tag.
func (w *world) imageBase(t *testing.T, tag string, saves *int) Subject {
	t.Helper()
	ref := "stackr/pg:" + tag
	return Subject{
		Volume: w.vol, Engine: w.api, Base: true,
		Dump: []string{"pg_basebackup"}, WalStart: []string{"psql", "walfile"},
		Image: &Image{
			Key: bk.ImageKey("o1", w.vol.ID, tag),
			Save: func(wr io.Writer) error {
				*saves++
				return w.fake.Save(ctx, ref, wr)
			},
		},
	}
}

func (w *world) baseExec(t *testing.T) {
	t.Helper()
	basegz := tarGz(t, "base_backup", "pgdata")
	w.fake.ExecFn = func(cmd []string) string {
		if slices.Contains(cmd, "walfile") {
			return seg3 + "\n"
		}
		return string(basegz)
	}
}

// The image is written once per tag, recorded on the base's run, and loads
// back through Docker with no registry in the way.
func TestBaseSavesImageOncePerTagAndLoadsBack(t *testing.T) {
	w := setup(t)
	w.baseExec(t)
	root := bk.PITRRoot("o1", w.vol.ID)
	var saves int
	sp := Spec{Dest: w.dest, Prefix: root + "/base", Trigger: "manual", PITRDays: 7}
	r, err := w.f.Backup(ctx, w.imageBase(t, "17.11-ext-amd64-aaaaaaaa", &saves), sp, io.Discard)
	must(t, err)
	key := bk.ImageKey("o1", w.vol.ID, "17.11-ext-amd64-aaaaaaaa")
	if r.ImageKey != key {
		t.Fatalf("run image key = %q, want %q", r.ImageKey, key)
	}
	if _, err := w.f.Backup(ctx, w.imageBase(t, "17.11-ext-amd64-aaaaaaaa", &saves), sp, io.Discard); err != nil {
		t.Fatal(err)
	}
	if saves != 1 {
		t.Errorf("image saved %d times for one tag, want 1", saves)
	}
	if got := readObject(t, w, key); string(got) != "dockerfake-image:stackr/pg:17.11-ext-amd64-aaaaaaaa" {
		t.Errorf("stored image = %q", got)
	}
	// Restore side: the registry copy is gone (Gone), the saved tar brings it back.
	ref := "stackr/pg:17.11-ext-amd64-aaaaaaaa"
	w.fake.Gone = []string{ref}
	err = w.f.LoadImage(ctx, w.dest.ID, key, func(rd io.Reader) error { return w.fake.Load(ctx, ref, rd) }, io.Discard)
	must(t, err)
	if len(w.fake.Gone) != 0 {
		t.Errorf("image still gone after the load: %v", w.fake.Gone)
	}
	// A tar that is not the image is refused by the load check.
	err = w.f.LoadImage(ctx, w.dest.ID, key, func(rd io.Reader) error { return w.fake.Load(ctx, "stackr/pg:other", rd) }, io.Discard)
	if err == nil {
		t.Error("a load of the wrong tag passed")
	}
}

// Several rebuilds: an image a retained base references stays, one no base
// references goes, and a base the window drops takes its image with it.
func TestImagePruningFollowsRetainedBases(t *testing.T) {
	w := setup(t)
	w.baseExec(t)
	root := bk.PITRRoot("o1", w.vol.ID)
	imgKey := func(tag string) string { return bk.ImageKey("o1", w.vol.ID, tag) }
	put := func(tag string) string {
		must(t, open(w.dest).Put(ctx, imgKey(tag), strings.NewReader("img")))
		return imgKey(tag)
	}
	tie := func(baseKey, imageKey string) {
		rs, err := w.f.Backups.RunsOn(ctx, w.dest.ID)
		must(t, err)
		for _, r := range rs {
			if r.ObjectKey == baseKey {
				r.ImageKey = imageKey
				must(t, w.st.BackupRuns.Update(ctx, r))
				return
			}
		}
		t.Fatalf("no run for %s", baseKey)
	}
	// 30 days: dropped by the 7 day window. 20 days: the anchor, kept.
	// 3 days: inside the window. Each had its own image (three rebuilds).
	tie(w.oldBase(t, root, 30, seg1), put("t30"))
	tie(w.oldBase(t, root, 20, seg2), put("t20"))
	tie(w.oldBase(t, root, 3, seg2), put("t3"))
	orphan := put("orphan") // no base references it
	var saves int
	r, err := w.f.Backup(ctx, w.imageBase(t, "tnew", &saves), Spec{Dest: w.dest, Prefix: root + "/base", Trigger: "manual", PITRDays: 7}, io.Discard)
	must(t, err)
	keys, _ := open(w.dest).List(ctx, root+"/image/")
	for _, k := range []string{imgKey("t20"), imgKey("t3"), r.ImageKey} {
		if !slices.Contains(keys, k) {
			t.Errorf("referenced image %s was pruned: %v", k, keys)
		}
	}
	for _, k := range []string{imgKey("t30"), orphan} {
		if slices.Contains(keys, k) {
			t.Errorf("unreferenced image %s survived: %v", k, keys)
		}
	}
}

// Purge removes the images of the runs it deletes, on a live org's
// destination too (its keys are run-row keys only).
func TestPurgeRemovesImages(t *testing.T) {
	w := setup(t)
	w.baseExec(t)
	root := bk.PITRRoot("o1", w.vol.ID)
	var saves int
	r, err := w.f.Backup(ctx, w.imageBase(t, "tp", &saves), Spec{Dest: w.dest, Prefix: root + "/base", Trigger: "manual", PITRDays: 7}, io.Discard)
	must(t, err)
	if keys, _ := open(w.dest).List(ctx, r.ImageKey); len(keys) != 1 {
		t.Fatalf("image not saved: %v", keys)
	}
	must(t, w.f.Purge(ctx, w.dest, false))
	if keys, _ := open(w.dest).List(ctx, root+"/image/"); len(keys) != 0 {
		t.Errorf("images left after Purge: %v", keys)
	}
}

// A subject with no Image (any instance not recipe-built) writes none and
// records no key.
func TestBaseWithoutImageIsUnchanged(t *testing.T) {
	w := setup(t)
	w.baseExec(t)
	root := bk.PITRRoot("o1", w.vol.ID)
	sub := Subject{Volume: w.vol, Engine: w.api, Base: true, Dump: []string{"pg_basebackup"}, WalStart: []string{"psql", "walfile"}}
	r, err := w.f.Backup(ctx, sub, Spec{Dest: w.dest, Prefix: root + "/base", Trigger: "manual", PITRDays: 7}, io.Discard)
	must(t, err)
	keys, _ := open(w.dest).List(ctx, root+"/")
	if r.ImageKey != "" || len(keys) != 1 || !bytes.HasPrefix([]byte(keys[0]), []byte(root+"/base/")) {
		t.Errorf("key %q, objects %v", r.ImageKey, keys)
	}
}

// A failed image save warns and keeps the base: a backup is never lost to
// its image.
func TestFailedImageSaveKeepsTheBase(t *testing.T) {
	w := setup(t)
	w.baseExec(t)
	root := bk.PITRRoot("o1", w.vol.ID)
	sub := Subject{
		Volume: w.vol, Engine: w.api, Base: true, Dump: []string{"pg_basebackup"}, WalStart: []string{"psql", "walfile"},
		Image: &Image{Key: bk.ImageKey("o1", w.vol.ID, "t1"), Save: func(io.Writer) error { return io.ErrUnexpectedEOF }},
	}
	var log strings.Builder
	r, err := w.f.Backup(ctx, sub, Spec{Dest: w.dest, Prefix: root + "/base", Trigger: "manual", PITRDays: 7}, &log)
	must(t, err)
	if r.ImageKey != "" || r.ObjectKey == "" {
		t.Errorf("image key %q, object %q", r.ImageKey, r.ObjectKey)
	}
	if !strings.Contains(log.String(), "warning: saving the image") {
		t.Errorf("no warning in %q", log.String())
	}
	// no half-written image left for the next base to take as saved
	if keys, _ := open(w.dest).List(ctx, root+"/image/"); len(keys) != 0 {
		t.Errorf("image objects after a failed save: %v", keys)
	}
}
