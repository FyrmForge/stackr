package volume_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// fakeDisk is the Disk seam over a map. Folder rows are shared by all nodes.
type fakeDisk struct {
	state    map[string]volume.DiskState
	stateErr error
	rows     map[string]bool
	use      map[string]volume.Use
	usageErr error
	dropErr  error
	placed   []string
	dropped  []string
	modes    []volume.UseMode
	usageOn  []string // node of each Usage call
}

func (d *fakeDisk) State(_ context.Context, node string) (volume.DiskState, error) {
	return d.state[node], d.stateErr
}

func (d *fakeDisk) Folders(_ context.Context, _ string, names ...string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = d.rows[n]
	}
	return out, nil
}

func (d *fakeDisk) Path(_ context.Context, node, name string) (string, error) {
	if d.state[node] != volume.DiskReady {
		return "", &volume.MissingError{Node: node}
	}
	return "/d/" + name, nil
}

func (d *fakeDisk) Place(_ context.Context, node, name string) (map[string]string, error) {
	if d.state[node] != volume.DiskReady {
		return nil, &volume.MissingError{Node: node}
	}
	d.rows[name] = true
	d.placed = append(d.placed, name)
	return docker.BindOpts("/d/" + name), nil
}

func (d *fakeDisk) Usage(_ context.Context, node string, m volume.UseMode, names ...string) (map[string]volume.Use, error) {
	d.modes = append(d.modes, m)
	d.usageOn = append(d.usageOn, node)
	out := map[string]volume.Use{}
	for _, n := range names {
		u, ok := d.use[n]
		if !ok {
			u = volume.Use{Missing: true, Empty: true}
		}
		out[n] = u
	}
	return out, d.usageErr
}

func (d *fakeDisk) Drop(_ context.Context, _, name string) error {
	if d.dropErr != nil {
		return d.dropErr
	}
	d.dropped = append(d.dropped, name)
	delete(d.rows, name)
	return nil
}

type rig struct {
	l    *volume.Leaf
	f    *dockerfake.Fake
	d    *fakeDisk
	st   *store.Store
	v    store.Volume
	name string
}

func newRig(t *testing.T, state volume.DiskState) *rig {
	t.Helper()
	st := servicetest.Store(t)
	f := dockerfake.New()
	d := &fakeDisk{
		state: map[string]volume.DiskState{"n1": state},
		rows:  map[string]bool{},
		use:   map[string]volume.Use{},
	}
	l := volume.New(st.Volumes, f).WithDisk(d)
	v, _, err := l.Declare(ctx, env, "data", nil)
	if err != nil {
		t.Fatal(err)
	}
	v.NodeID = "n1"
	return &rig{l: l, f: f, d: d, st: st, v: v, name: v.Name}
}

func (r *rig) calls() string {
	var s []string
	for _, c := range r.f.Calls() {
		s = append(s, c.String())
	}
	return strings.Join(s, " ")
}

func (r *rig) has(method string) bool { return strings.Contains(r.calls(), method+"(") }

// bound makes the Docker object exist bound to dev; plain makes it a plain one.
func (r *rig) bound(dev string) {
	_ = r.f.CreateVolume(ctx, r.name, "local", docker.BindOpts(dev), nil)
	r.f.Volumes = []docker.VolumeInfo{{Name: r.name}}
}

func (r *rig) plain(size int64) {
	r.f.Volumes = []docker.VolumeInfo{{Name: r.name, SizeBytes: size}}
}

func (r *rig) rowGone(t *testing.T) bool {
	t.Helper()
	_, err := r.l.Get(ctx, r.v.ID)
	return errors.Is(err, errs.ErrNotFound)
}

func isMissing(err error) bool { var m *volume.MissingError; return errors.As(err, &m) }
func isGhost(err error) bool   { var g *volume.GhostError; return errors.As(err, &g) }

func TestEnsureNewVolumeGoesToDisk(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	if _, err := r.l.Ensure(ctx, r.v); err != nil {
		t.Fatal(err)
	}
	c := r.f.Created
	if len(c) != 1 || docker.DeviceOf(c[0].Opts) != "/d/"+r.name || c[0].Labels["stackr.volume"] != r.v.ID {
		t.Errorf("created = %+v", c)
	}
	if !slices.Equal(r.d.placed, []string{r.name}) {
		t.Errorf("placed = %v", r.d.placed)
	}
}

func TestEnsureLegacyVolumeStays(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.plain(5)
	if _, err := r.l.Ensure(ctx, r.v); err != nil {
		t.Fatal(err)
	}
	if len(r.d.placed) != 0 || len(r.f.Created) != 1 || r.f.Created[0].Opts != nil {
		t.Errorf("placed %v created %+v", r.d.placed, r.f.Created)
	}
}

// Compat: a node with no disk row makes today's one CreateVolume, no inspect.
func TestEnsureNoRowIsTodaysCall(t *testing.T) {
	r := newRig(t, volume.DiskNone)
	if _, err := r.l.Ensure(ctx, r.v); err != nil {
		t.Fatal(err)
	}
	want := dockerfake.VolumeCreate{Name: r.name, Driver: "local", Labels: map[string]string{"stackr.volume": r.v.ID}}
	if len(r.f.Created) != 1 || r.f.Created[0].Name != want.Name || r.f.Created[0].Driver != "local" ||
		r.f.Created[0].Opts != nil || r.f.Created[0].Labels["stackr.volume"] != r.v.ID {
		t.Errorf("created = %+v", r.f.Created)
	}
	if got := r.calls(); got != "CreateVolume("+r.name+")" {
		t.Errorf("calls = %s", got)
	}
}

// DL
func TestEnsureOnLatchedDiskWithAbsentObjectReturnsMissingAndCreatesNothing(t *testing.T) {
	for _, st := range []volume.DiskState{volume.DiskNotReady} {
		r := newRig(t, st)
		_, err := r.l.Ensure(ctx, r.v)
		if !isMissing(err) || !errors.Is(err, errs.ErrBusy) {
			t.Errorf("err = %v", err)
		}
		if len(r.f.Created) != 0 || len(r.d.placed) != 0 {
			t.Errorf("created %+v placed %v", r.f.Created, r.d.placed)
		}
	}
}

// DL
func TestUsageOnLatchedDiskIsMissingNotDockerZero(t *testing.T) {
	r := newRig(t, volume.DiskNotReady)
	r.d.rows[r.name] = true
	r.plain(0) // Docker says zero
	if !r.l.HoldsData(ctx, r.v) {
		t.Error("latched disk read as empty")
	}
	if got := r.l.SizeBytes(ctx, r.v); got != -1 {
		t.Errorf("size = %d", got)
	}
	if got := r.l.Sizes(ctx, []store.Volume{r.v})[r.v.ID]; got != -1 {
		t.Errorf("sizes = %d", got)
	}
}

// DL
func TestDeleteOnLatchedDiskFailsAndKeepsRow(t *testing.T) {
	r := newRig(t, volume.DiskNotReady)
	r.d.rows[r.name] = true
	if err := r.l.Delete(ctx, r.v, nil); !isMissing(err) {
		t.Errorf("err = %v", err)
	}
	if r.rowGone(t) || r.has("RemoveVolume") {
		t.Error("delete went ahead on a latched disk")
	}
}

func TestEnsureEmptyGhostIsRemovedAndRebound(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	r.plain(0)
	if _, err := r.l.Ensure(ctx, r.v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.calls(), "RemoveVolume("+r.name+") CreateVolume("+r.name+")") {
		t.Errorf("calls = %s", r.calls())
	}
	if c := r.f.Created[len(r.f.Created)-1]; docker.DeviceOf(c.Opts) != "/d/"+r.name {
		t.Errorf("rebound opts = %v", c.Opts)
	}
}

// DL
func TestEnsureNonEmptyGhostRefuses(t *testing.T) {
	for _, size := range []int64{7, -1} {
		r := newRig(t, volume.DiskReady)
		r.d.rows[r.name] = true
		r.plain(size)
		_, err := r.l.Ensure(ctx, r.v)
		if !isGhost(err) {
			t.Errorf("size %d: err = %v", size, err)
		}
		if r.has("RemoveVolume") || len(r.f.Created) != 0 {
			t.Errorf("size %d: ghost touched: %s", size, r.calls())
		}
	}
}

func TestEnsureRecreatesObjectForExistingFolder(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	if _, err := r.l.Ensure(ctx, r.v); err != nil {
		t.Fatal(err)
	}
	if len(r.f.Created) != 1 || docker.DeviceOf(r.f.Created[0].Opts) != "/d/"+r.name {
		t.Errorf("created = %+v", r.f.Created)
	}
}

// DL
func TestHoldsDataTrueWhenDockerSaysZeroButFolderHasFiles(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	r.bound("/d/" + r.name)
	r.d.use[r.name] = volume.Use{Bytes: 9}
	if !r.l.HoldsData(ctx, r.v) {
		t.Error("files in the folder read as empty")
	}
	if r.has("InspectVolume") {
		t.Error("Docker's size was consulted for a disk volume")
	}
}

// DL
func TestHoldsDataTrueOnUsageErrorAndOnMissingRoute(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	r.bound("/d/" + r.name)
	r.d.use[r.name] = volume.Use{Empty: true}
	r.d.usageErr = errors.New("helper down")
	if !r.l.HoldsData(ctx, r.v) {
		t.Error("usage error read as empty")
	}
	r.d.usageErr = nil
	delete(r.d.use, r.name) // the folder is gone behind a row
	if !r.l.HoldsData(ctx, r.v) {
		t.Error("row with a missing folder read as empty")
	}
	r.d.state["n1"] = volume.DiskNotReady
	r.d.use[r.name] = volume.Use{Empty: true}
	if !r.l.HoldsData(ctx, r.v) {
		t.Error("not-ready disk read as empty")
	}
}

func TestHoldsDataFalseOnlyOnVerifiedEmpty(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	r.bound("/d/" + r.name)
	r.d.use[r.name] = volume.Use{Empty: true}
	if r.l.HoldsData(ctx, r.v) {
		t.Error("verified empty folder holds data")
	}
}

// A ready disk, no folder row and no Docker object: never created, no data.
func TestHoldsDataFalseWhenNeverCreatedOnReadyDisk(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	if r.l.HoldsData(ctx, r.v) {
		t.Error("a volume that never existed holds data")
	}
	if len(r.d.usageOn) != 0 || len(r.d.placed) != 0 {
		t.Error("asked the disk about nothing")
	}
}

// DL
func TestDeleteRemovesDockerVolumeThenFolderThenRow(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	r.bound("/d/" + r.name)
	if err := r.l.Delete(ctx, r.v, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.d.dropped, []string{r.name}) || r.d.rows[r.name] || !r.rowGone(t) || !r.has("RemoveVolume") {
		t.Errorf("dropped %v rows %v calls %s", r.d.dropped, r.d.rows, r.calls())
	}
	// A failed drop keeps the row.
	r = newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	r.bound("/d/" + r.name)
	r.d.dropErr = errors.New("io")
	if err := r.l.Delete(ctx, r.v, nil); err == nil || r.rowGone(t) {
		t.Errorf("failed drop: err %v rowGone %v", err, r.rowGone(t))
	}
}

// DL
func TestDeleteLeavesNoFolder(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	if _, err := r.l.Ensure(ctx, r.v); err != nil {
		t.Fatal(err)
	}
	if err := r.l.Delete(ctx, r.v, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.d.rows) != 0 || !slices.Equal(r.d.dropped, []string{r.name}) {
		t.Errorf("rows %v dropped %v", r.d.rows, r.d.dropped)
	}
}

// DeleteRow is the row only, for a node that is gone.
func TestDeleteRowTouchesNeitherDockerNorDisk(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	if err := r.l.DeleteRow(ctx, r.v, nil); err != nil {
		t.Fatal(err)
	}
	if !r.rowGone(t) || len(r.f.Calls()) != 0 || len(r.d.dropped) != 0 || !r.d.rows[r.name] {
		t.Errorf("calls %s dropped %v", r.calls(), r.d.dropped)
	}
}

// DL
func TestTarEnsuresFirstOnlyWhenTheNodeHasADiskRow(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	if err := r.l.Tar(ctx, r.v, &bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.calls(), "CreateVolume("+r.name+") TarVolume("+r.name+")") ||
		docker.DeviceOf(r.f.Created[0].Opts) != "/d/"+r.name {
		t.Errorf("calls = %s created %+v", r.calls(), r.f.Created)
	}
	r = newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	if err := r.l.Untar(ctx, r.v, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.calls(), "CreateVolume("+r.name+") UntarVolume("+r.name+")") {
		t.Errorf("untar calls = %s", r.calls())
	}
}

func TestTarWithoutARowIsTodaysCall(t *testing.T) {
	r := newRig(t, volume.DiskNone)
	if err := r.l.Tar(ctx, r.v, &bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if err := r.l.Untar(ctx, r.v, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if got := r.calls(); got != "TarVolume("+r.name+") UntarVolume("+r.name+")" {
		t.Errorf("calls = %s", got)
	}
}

// A node with no disk row: the same Docker calls as before the seam, no
// Folders, Usage or inspect-by-device.
func TestNodeWithoutDiskUnchanged(t *testing.T) {
	r := newRig(t, volume.DiskNone)
	r.plain(3)
	r.l.HoldsData(ctx, r.v)
	r.l.SizeBytes(ctx, r.v)
	r.l.Sizes(ctx, []store.Volume{r.v})
	_ = r.l.RemoveNamed(ctx, "n1", r.name)
	if err := r.l.Delete(ctx, r.v, nil); err != nil {
		t.Fatal(err)
	}
	if got := r.calls(); got != "InspectVolume("+r.name+") InspectVolume("+r.name+") ListVolumes() RemoveVolume("+r.name+") RemoveVolume("+r.name+")" {
		t.Errorf("calls = %s", got)
	}
	if len(r.d.usageOn) != 0 {
		t.Error("disk asked on a node with no disk")
	}
}

func TestSizesMixDiskAndDocker(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	r.bound("/d/" + r.name)
	r.d.use[r.name] = volume.Use{Bytes: 42}
	pl, _, _ := r.l.Declare(ctx, env, "plain", nil)
	pl.NodeID = "n1"
	r.f.Volumes = append(r.f.Volumes, docker.VolumeInfo{Name: pl.Name, SizeBytes: 7})
	got := r.l.Sizes(ctx, []store.Volume{r.v, pl})
	if got[r.v.ID] != 42 || got[pl.ID] != 7 {
		t.Errorf("sizes = %v", got)
	}
	if r.l.SizeBytes(ctx, r.v) != 42 {
		t.Error("SizeBytes ignores the disk")
	}
}

func TestSizesGroupByNodeAfterARelocation(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.state["n2"] = volume.DiskNone
	r.d.rows[r.name] = true
	r.bound("/d/" + r.name)
	r.d.use[r.name] = volume.Use{Bytes: 5}
	other, _, _ := r.l.Declare(ctx, env, "moved", nil)
	other.NodeID = "n2"
	r.f.Volumes = append(r.f.Volumes, docker.VolumeInfo{Name: other.Name, SizeBytes: 9})
	got := r.l.Sizes(ctx, []store.Volume{other, r.v}) // the first lives on n2
	if got[r.v.ID] != 5 || got[other.ID] != 9 {
		t.Errorf("sizes = %v", got)
	}
	if !slices.Equal(r.d.usageOn, []string{"n1"}) {
		t.Errorf("usage nodes = %v", r.d.usageOn)
	}
}

func TestSizesUseTheCacheAndNeverRunTheHelperFromAPoll(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	r.bound("/d/" + r.name)
	r.l.Sizes(ctx, []store.Volume{r.v})
	r.l.SizeBytes(ctx, r.v)
	r.l.HoldsData(ctx, r.v)
	if !slices.Equal(r.d.modes, []volume.UseMode{volume.UseCached, volume.UseCached, volume.UseEmpty}) {
		t.Errorf("modes = %v", r.d.modes)
	}
}

// DL
func TestStateLookupErrorFailsClosed(t *testing.T) {
	r := newRig(t, volume.DiskNone)
	r.d.stateErr = errors.New("db down")
	r.d.rows[r.name] = true
	if _, err := r.l.Ensure(ctx, r.v); !errors.Is(err, r.d.stateErr) {
		t.Errorf("ensure = %v", err)
	}
	if len(r.f.Created) != 0 {
		t.Error("created on a failed lookup")
	}
	if !r.l.HoldsData(ctx, r.v) {
		t.Error("HoldsData false on a failed lookup")
	}
	if r.l.SizeBytes(ctx, r.v) != -1 || r.l.Sizes(ctx, []store.Volume{r.v})[r.v.ID] != -1 {
		t.Error("size known on a failed lookup")
	}
	if err := r.l.Delete(ctx, r.v, nil); !errors.Is(err, r.d.stateErr) || r.rowGone(t) {
		t.Errorf("delete = %v", err)
	}
	if err := r.l.RemoveNamed(ctx, "n1", r.name); !errors.Is(err, r.d.stateErr) {
		t.Errorf("remove = %v", err)
	}
	if err := r.l.Tar(ctx, r.v, &bytes.Buffer{}, false); !errors.Is(err, r.d.stateErr) {
		t.Errorf("tar = %v", err)
	}
	if err := r.l.Untar(ctx, r.v, strings.NewReader("x")); !errors.Is(err, r.d.stateErr) {
		t.Errorf("untar = %v", err)
	}
	if len(r.f.Calls()) != 0 {
		t.Errorf("docker touched: %s", r.calls())
	}
}

// DL: both bound-elsewhere cells, plus the plain ghost's non-empty case.
func TestGhostRouteEveryVerbFailsSafe(t *testing.T) {
	for _, hasRow := range []bool{false, true} {
		r := newRig(t, volume.DiskReady)
		r.d.rows[r.name] = hasRow
		r.bound("/elsewhere/x")
		if !r.l.HoldsData(ctx, r.v) {
			t.Errorf("row %v: ghost holds no data", hasRow)
		}
		if r.l.SizeBytes(ctx, r.v) != -1 || r.l.Sizes(ctx, []store.Volume{r.v})[r.v.ID] != -1 {
			t.Errorf("row %v: ghost size known", hasRow)
		}
		check := func(what string, err error) {
			var g *volume.GhostError
			if !errors.As(err, &g) || g.Device != "/elsewhere/x" || g.Name != r.name {
				t.Errorf("row %v: %s = %v", hasRow, what, err)
			}
		}
		_, err := r.l.Ensure(ctx, r.v)
		check("ensure", err)
		check("delete", r.l.Delete(ctx, r.v, nil))
		check("remove", r.l.RemoveNamed(ctx, "n1", r.name))
		check("tar", r.l.Tar(ctx, r.v, &bytes.Buffer{}, false))
		check("untar", r.l.Untar(ctx, r.v, strings.NewReader("x")))
		if r.rowGone(t) || r.has("RemoveVolume") || r.has("TarVolume") || r.has("UntarVolume") {
			t.Errorf("row %v: ghost touched: %s", hasRow, r.calls())
		}
	}
}

// V1: a read of a bound object on a ready disk never places: no host call, no row.
func TestReadsOfABoundObjectNeverPlace(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.bound("/d/" + r.name) // bound at the right folder, no row yet
	r.d.use[r.name] = volume.Use{Bytes: 3}
	r.l.Sizes(ctx, []store.Volume{r.v})
	r.l.SizeBytes(ctx, r.v)
	r.l.HoldsData(ctx, r.v)
	if len(r.d.placed) != 0 || r.d.rows[r.name] {
		t.Errorf("a read placed: %v rows %v", r.d.placed, r.d.rows)
	}
	// Sizes and SizeBytes read the cache, HoldsData only a readdir: no read asks for a fresh walk
	if want := []volume.UseMode{volume.UseCached, volume.UseCached, volume.UseEmpty}; !slices.Equal(r.d.modes, want) {
		t.Errorf("modes = %v, want %v", r.d.modes, want)
	}
}

// V6: removing what is not there is fine on a node whose disk is missing.
func TestRemoveNamedOfNothingSucceedsOnAMissingDisk(t *testing.T) {
	r := newRig(t, volume.DiskNotReady) // no Docker object, no folder row
	if err := r.l.RemoveNamed(ctx, "n1", r.name); err != nil {
		t.Fatalf("remove of nothing: %v", err)
	}
	r.d.rows[r.name] = true // a folder row still refuses
	if err := r.l.RemoveNamed(ctx, "n1", r.name); !isMissing(err) {
		t.Fatalf("remove with a row: %v", err)
	}
}

func TestSizeBytesFreshAsksTheHelperNotTheCache(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	r.bound("/d/" + r.name)
	r.d.use[r.name] = volume.Use{Bytes: 42}
	if got := r.l.SizeBytesFresh(ctx, r.v); got != 42 {
		t.Fatalf("fresh size = %d", got)
	}
	if !slices.Equal(r.d.modes, []volume.UseMode{volume.UseFresh}) {
		t.Errorf("modes = %v", r.d.modes)
	}
}

// R11: SizesFresh is one fresh usage call per node, however many volumes.
func TestSizesFreshIsOneFreshCallPerNode(t *testing.T) {
	r := newRig(t, volume.DiskReady)
	r.d.rows[r.name] = true
	r.bound("/d/" + r.name)
	r.d.use[r.name] = volume.Use{Bytes: 42}
	got := r.l.SizesFresh(ctx, []store.Volume{r.v})
	if got[r.v.ID] != 42 || !slices.Equal(r.d.modes, []volume.UseMode{volume.UseFresh}) {
		t.Fatalf("sizes %v modes %v", got, r.d.modes)
	}
}
