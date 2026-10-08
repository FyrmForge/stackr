package volume_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

// Each rule of a share row refuses; a good row of each kind passes.
func TestCheckShare(t *testing.T) {
	ok := func(sp volume.ShareSpec) volume.ShareSpec { return sp }
	good := []volume.ShareSpec{
		ok(volume.ShareSpec{Slug: "media", Kind: "nfs", Source: "nas:/export", Options: "nfsvers=4"}),
		ok(volume.ShareSpec{Slug: "docs", Kind: "smb", Source: "//nas/docs/x", User: "bob",
			PasswordRef: "${{ org.params.nas.pw }}"}),
		ok(volume.ShareSpec{Slug: "pub", Kind: "smb", Source: "//nas/pub"}),
	}
	for _, sp := range good {
		if err := volume.CheckShare(sp); err != nil {
			t.Errorf("%+v: %v", sp, err)
		}
	}
	for name, sp := range map[string]volume.ShareSpec{
		"bad slug":        {Slug: "Bad Name", Kind: "nfs", Source: "nas:/e"},
		"bad kind":        {Slug: "a", Kind: "ftp", Source: "nas:/e"},
		"nfs source":      {Slug: "a", Kind: "nfs", Source: "//nas/e"},
		"smb source":      {Slug: "a", Kind: "smb", Source: "nas:/e"},
		"nfs creds":       {Slug: "a", Kind: "nfs", Source: "nas:/e", User: "bob"},
		"user no pass":    {Slug: "a", Kind: "smb", Source: "//nas/e", User: "bob"},
		"literal pass":    {Slug: "a", Kind: "smb", Source: "//nas/e", User: "bob", PasswordRef: "hunter2"},
		"server pass":     {Slug: "a", Kind: "smb", Source: "//nas/e", User: "bob", PasswordRef: "${{ server.params.nas.pw }}"},
		"server user":     {Slug: "a", Kind: "smb", Source: "//nas/e", User: "${{ server.params.nas.user }}", PasswordRef: "${{ org.params.nas.pw }}"},
		"options space":   {Slug: "a", Kind: "nfs", Source: "nas:/e", Options: "ro nolock"},
		"options device":  {Slug: "a", Kind: "nfs", Source: "nas:/e", Options: "device=/x"},
		"options secrets": {Slug: "a", Kind: "smb", Source: "//nas/e", Options: "password=x"},
	} {
		if err := volume.CheckShare(sp); err == nil {
			t.Errorf("%s: accepted %+v", name, sp)
		}
	}
}

// A slug is the org's own; another org may reuse it; updates keep id and slug.
func TestShareRows(t *testing.T) {
	st := storetest.Store(t)
	l := volume.New(st.Volumes, dockerfake.New()).WithShares(st.Shares)
	for _, id := range []string{"o1", "o2"} {
		if err := st.Orgs.Create(ctx, store.Org{ID: id, Name: id, Slug: id, EnvColors: "{}", Settings: "{}",
			CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	sp := volume.ShareSpec{Slug: "media", Kind: "nfs", Source: "nas:/e"}
	s, err := l.CreateShare(ctx, "o1", sp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.CreateShare(ctx, "o1", sp); err == nil {
		t.Error("a duplicate slug in one org was accepted")
	}
	if _, err := l.CreateShare(ctx, "o2", sp); err != nil {
		t.Errorf("another org's slug: %v", err)
	}
	if _, err := l.ShareOf(ctx, "o2", s.ID); err == nil {
		t.Error("o2 reached o1's share by id")
	}
	sp.Source = "nas2:/e"
	up, err := l.UpdateShare(ctx, s, sp)
	if err != nil || up.ID != s.ID || up.Source != "nas2:/e" {
		t.Errorf("update = %+v, %v", up, err)
	}
	if err := l.DeleteShare(ctx, s, []string{"web"}); err == nil {
		t.Error("deleted while mounted")
	}
	if err := l.DeleteShare(ctx, s, nil); err != nil {
		t.Error(err)
	}
}

// TestDeleteShareDropsVolumes: the share's Docker volumes go with the row;
// one a container still holds refuses the delete and keeps the row.
func TestDeleteShareDropsVolumes(t *testing.T) {
	st := storetest.Store(t)
	d := dockerfake.New()
	l := volume.New(st.Volumes, d).WithShares(st.Shares)
	if err := st.Orgs.Create(ctx, store.Org{ID: "o1", Name: "o1", Slug: "o1", EnvColors: "{}", Settings: "{}",
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s, err := l.CreateShare(ctx, "o1", volume.ShareSpec{Slug: "media", Kind: "nfs", Source: "nas:/e"})
	if err != nil {
		t.Fatal(err)
	}
	d.Volumes = []docker.VolumeInfo{
		{Name: "stackr-share-" + s.ID + "-a", Labels: map[string]string{volume.ShareLabel: s.ID}},
		{Name: "stackr-share-other-b", Labels: map[string]string{volume.ShareLabel: "other"}},
		{Name: "stackr-vol-c", Labels: map[string]string{"stackr.volume": "c"}},
	}
	d.Err = map[string]error{"RemoveVolume": errors.New("volume is in use")}
	if _, ok := errs.IsConflict(l.DeleteShare(ctx, s, nil)); !ok {
		t.Fatal("a held volume did not refuse the delete")
	}
	if _, err := l.ShareOf(ctx, "o1", s.ID); err != nil {
		t.Fatalf("row gone after a refused delete: %v", err)
	}
	delete(d.Err, "RemoveVolume")
	if err := l.DeleteShare(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, c := range d.Calls() {
		if c.Method == "RemoveVolume" {
			removed = append(removed, c.Args[0])
		}
	}
	// Two calls: the refused one, then the real one; only this share's volume.
	if len(removed) != 2 || removed[1] != "stackr-share-"+s.ID+"-a" {
		t.Errorf("removed %v", removed)
	}
	if _, err := l.ShareOf(ctx, "o1", s.ID); err == nil {
		t.Error("row still there")
	}
}

// shareWorld is org o1 with an NFS share "media" and docker volumes labelled
// for it: one a tile row still names, one nothing names.
func sweepWorld(t *testing.T) (*volume.Leaf, *dockerfake.Fake, store.Share, string, string) {
	t.Helper()
	st := storetest.Store(t)
	d := dockerfake.New()
	l := volume.New(st.Volumes, d).WithShares(st.Shares)
	if err := st.Orgs.Create(ctx, store.Org{ID: "o1", Name: "o1", Slug: "o1", EnvColors: "{}", Settings: "{}",
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s, err := l.CreateShare(ctx, "o1", volume.ShareSpec{Slug: "media", Kind: "nfs", Source: "nas:/e"})
	if err != nil {
		t.Fatal(err)
	}
	live, gone := volume.ShareVolumeName(s, "a"), volume.ShareVolumeName(s, "b")
	lbl := map[string]string{volume.ShareLabel: s.ID}
	d.Volumes = []docker.VolumeInfo{
		{Name: live, Labels: lbl},
		{Name: gone, Labels: lbl},
		{Name: "stackr-share-other-x", Labels: map[string]string{volume.ShareLabel: "other"}},
		{Name: "stackr-vol-c", Labels: map[string]string{"stackr.volume": "c"}},
	}
	return l, d, s, live, gone
}

func removedVolumes(d *dockerfake.Fake) []string {
	var out []string
	for _, c := range d.Calls() {
		if c.Method == "RemoveVolume" {
			out = append(out, c.Args[0])
		}
	}
	return out
}

// A deploy-made volume of a tile row that still names it survives a sweep
// even though no container holds it yet (the image is still pulling); the
// volume no row names goes; another org's and plain volumes are untouched.
func TestSweepSharesKeepsRowVolumes(t *testing.T) {
	l, d, _, _, gone := sweepWorld(t)
	held, err := l.SweepShares(ctx, "o1", []volume.ShareUse{{Share: "media", Sub: "a"}})
	if err != nil || len(held) != 0 {
		t.Fatalf("sweep = %v, %v", held, err)
	}
	if got := removedVolumes(d); len(got) != 1 || got[0] != gone {
		t.Errorf("removed %v, want only %s", got, gone)
	}
}

// Docker's in-use refusal means held (named, row-less volume stays); any
// other error comes back as an error.
func TestSweepSharesErrors(t *testing.T) {
	l, d, _, _, gone := sweepWorld(t)
	d.Err = map[string]error{"RemoveVolume": errors.New("remove x: volume is in use")}
	held, err := l.SweepShares(ctx, "o1", nil)
	if err != nil || len(held) != 2 {
		t.Fatalf("in-use sweep = %v, %v", held, err)
	}
	if !slices.Contains(held, gone) {
		t.Errorf("held %v lacks %s", held, gone)
	}
	d.Err = map[string]error{"RemoveVolume": errors.New("Cannot connect to the Docker daemon")}
	if _, err = l.SweepShares(ctx, "o1", nil); err == nil {
		t.Error("a daemon error was reported as held")
	}
	d.Err = map[string]error{"RemoveVolume": context.Canceled}
	if _, err = l.SweepShares(ctx, "o1", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled sweep err = %v", err)
	}
}

// DeleteShare: only the in-use refusal is the "a container holds it" conflict.
func TestDeleteShareOtherErrors(t *testing.T) {
	l, d, s, _, _ := sweepWorld(t)
	d.Err = map[string]error{"RemoveVolume": errors.New("Cannot connect to the Docker daemon")}
	err := l.DeleteShare(ctx, s, nil)
	if err == nil {
		t.Fatal("delete went through")
	}
	if _, ok := errs.IsConflict(err); ok {
		t.Errorf("a daemon error became a conflict: %v", err)
	}
}

// The volume name follows the share's refs, not the expanded secret: a
// rotated password mounts the same volume.
func TestShareVolumeNameIgnoresSecretValue(t *testing.T) {
	l, d, _, _, _ := sweepWorld(t)
	s := store.Share{ID: "s1", Slug: "docs", Kind: volume.SMB, Source: "//nas/docs", User: "bob",
		PasswordRef: "${{ org.params.nas.pw }}"}
	a, err := l.EnsureShare(ctx, s, "x", "bob", "old")
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.EnsureShare(ctx, s, "x", "bob", "new")
	if err != nil || a != b {
		t.Errorf("names %q, %q (%v)", a, b, err)
	}
	if a != volume.ShareVolumeName(s, "x") {
		t.Errorf("%q is not ShareVolumeName", a)
	}
	_ = d
	s.PasswordRef = "${{ org.params.nas.pw2 }}"
	if volume.ShareVolumeName(s, "x") == a {
		t.Error("another password ref kept the name")
	}
}
