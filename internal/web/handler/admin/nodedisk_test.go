package admin_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// diskNode is the controller's id: the one node a webtest site has.
func diskNode(t *testing.T, s *webtest.Site) string {
	t.Helper()
	ns, err := s.Orch.Nodes(t.Context())
	if err != nil || len(ns) != 1 {
		t.Fatalf("nodes = %+v, %v", ns, err)
	}
	return ns[0].ID
}

func has(t *testing.T, where, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s: no %q in\n%s", where, w, body)
		}
	}
}

// addedDisk makes dir (a mount in the rig) the controller's ready disk,
// through the dialog's own two POSTs.
func addedDisk(t *testing.T, s *webtest.Site, root, id, dir string) {
	t.Helper()
	s.Disk.Mount(t, dir)
	base := "/-/admin/nodes/" + id + "/disk/"
	f := url.Values{"path": {dir}}
	if rec := s.As(t, root, "POST", base+"probe", f); !strings.Contains(rec.Body.String(), `id="disk-checked"`) {
		t.Fatalf("probe = %d\n%s", rec.Code, rec.Body)
	}
	f.Set("checked", dir)
	if rec := s.As(t, root, "POST", base+"add", f); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Adding the disk.") {
		t.Fatalf("add = %d\n%s", rec.Code, rec.Body)
	}
	for range 200 {
		if v, err := s.Orch.NodeDisk(t.Context(), id); err == nil && v.State == "ready" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the disk never went ready")
}

// section is the Node disk card of a drawer answer.
func section(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `>Node disk</h2>`)
	if !ok {
		t.Fatalf("no Node disk section in\n%s", body)
	}
	card, _, _ := strings.Cut(rest, "</section>")
	return card
}

// No disk: the section offers Add disk, sits in a live region of its own
// poller, and holds no box inside the card.
func TestNodeDiskSectionOffersAddDisk(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	rec := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil)
	body := rec.Body.String()
	has(t, "overview", body,
		`id="node-live"`, `hx-select="#node-live"`, `hx-trigger="every 3s"`, // components.LiveTab
		"Keep volumes on their own disk, apart from the system disk.", "System disk")
	card := section(t, body)
	has(t, "section", card, ">Add disk</button>", `id="disk-use"`, `id="disk-path"`, `<label class="label" for="disk-path">Folder</label>`,
		`aria-live="polite"`, "A mounted, empty folder. Nothing is erased.", "Checking the folder...", `hx-indicator="#disk-checking"`, `id="disk-checking"`, "group-has-[#disk-path:placeholder-shown]/f:opacity-50")
	if strings.Contains(card, `class="panel`) || strings.Contains(card, "rounded-lg border") {
		t.Errorf("a box inside the Node disk card\n%s", card)
	}
	if strings.Contains(card, `<button type="button" id="disk-use" class="btn btn-primary btn-sm disabled:opacity-50 disabled:cursor-not-allowed" hx-post`) &&
		!strings.Contains(card, "disabled") {
		t.Error("Use folder starts enabled")
	}
	if strings.Contains(card, "data-confirm") {
		t.Error("Use folder is data-confirm: confirm-dialog would enable it on every open")
	}
}

// The dialog's refusals read in words, a network disk gets its warning and
// a box that must be ticked, and a refused add lands in the dialog.
func TestNodeDiskDialogRefusalsAndNetworkWarning(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	base := "/-/admin/nodes/" + id + "/disk/"
	post := func(op string, f url.Values) string {
		t.Helper()
		rec := s.As(t, root, "POST", base+op, f)
		if rec.Code != 200 {
			t.Fatalf("%s %v = %d\n%s", op, f, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}

	// a plain folder: not a mount point; a relative path; one that is not there
	must(t, os.MkdirAll(s.Disk.Dir("/plain"), 0o755))
	has(t, "plain", post("probe", url.Values{"path": {"/plain"}}), "Not a mount point. Mount the disk there first.", `aria-invalid="true"`, "disabled")
	has(t, "relative", post("probe", url.Values{"path": {"mnt/disk"}}), "Start the path with /.")
	has(t, "gone", post("probe", url.Values{"path": {"/nowhere"}}), "No such folder.")

	// a read-only mount is refused with its own words
	s.Disk.Mount(t, "/ro")
	tbl, err := os.ReadFile(s.Disk.Mountinfo)
	must(t, err)
	must(t, os.WriteFile(s.Disk.Mountinfo, []byte(strings.ReplaceAll(string(tbl), " /ro rw - ", " /ro ro - ")), 0o644))
	has(t, "read-only", post("probe", url.Values{"path": {"/ro"}}), "Read-only. Mount it writable.")

	// an empty mount passes: the result, and Use folder enabled
	s.Disk.Mount(t, "/data")
	ok := post("probe", url.Values{"path": {"/data"}})
	has(t, "ok", ok, "Mounted, empty,", ` value="/data"`)
	if strings.Contains(ok, `id="disk-use" class="btn btn-primary btn-sm disabled:opacity-50 disabled:cursor-not-allowed" hx-post="`+base+`add" hx-include="closest form" hx-target="#drawer-root" hx-swap="outerHTML" hx-disabled-elt="this" disabled`) {
		t.Errorf("a passed check leaves Use folder disabled\n%s", ok)
	}

	// a network mount: the warning and the box; Use folder waits for the tick
	s.Disk.Mount(t, "/nas")
	tbl, err = os.ReadFile(s.Disk.Mountinfo)
	must(t, err)
	must(t, os.WriteFile(s.Disk.Mountinfo, []byte(strings.ReplaceAll(string(tbl), "ext4 /dev/sdd", "nfs4 nas:/export")), 0o644))
	net := post("probe", url.Values{"path": {"/nas"}})
	has(t, "network", net, "Network disk (nfs4).", "Databases on a network disk risk corruption and slow writes.", "Use it anyway", `id="disk-ack"`)
	if !strings.Contains(net, "disabled") {
		t.Errorf("Use folder is enabled before the box is ticked\n%s", net)
	}
	// editing the path drops the answer; the helper is not asked
	cleared := post("state", url.Values{"path": {"/nas2"}, "checked": {"/nas"}, "net": {"1"}})
	if strings.Contains(cleared, "Network disk") || !strings.Contains(cleared, `id="disk-checked" name="checked" value=""`) {
		t.Errorf("a stale answer stays\n%s", cleared)
	}
	// an add the check did not pass, or a network disk not ticked, is refused in the dialog
	has(t, "unchecked", post("add", url.Values{"path": {"/data"}}), "Check the folder first.")
	has(t, "unticked", post("add", url.Values{"path": {"/nas"}, "checked": {"/nas"}, "net": {"1"}}), "Tick the box to use a network disk.")

	// the first disk is added; a second is refused in the dialog, not the drawer
	addedDisk(t, s, root, id, "/first")
	rec := s.As(t, root, "POST", base+"add", url.Values{"path": {"/data"}, "checked": {"/data"}})
	has(t, "second", rec.Body.String(), "This node already has a disk.", `aria-invalid="true"`)
	if rec.Header().Get("HX-Reswap") != "none" {
		t.Errorf("a refused add swaps the drawer: HX-Reswap = %q", rec.Header().Get("HX-Reswap"))
	}
}

// A disk that is not there: the state chip, the strip with Check disk, the
// forget hint, the Nodes list chip. Check disk queues the check.
func TestNodeDiskMissingOffersCheckDisk(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	addedDisk(t, s, root, id, "/data")

	rec := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil)
	has(t, "ready", rec.Body.String(), "New volumes on this node are stored here.", "Ready", "/data", "No volumes on it yet.")
	if strings.Contains(rec.Body.String(), "Check disk") {
		t.Error("Check disk offered on a ready disk")
	}

	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	d.State = "missing"
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	rec = s.As(t, root, "GET", "/-/admin/nodes/"+id, nil)
	body := rec.Body.String()
	has(t, "missing", body, "Disk missing", "The disk is missing.", "Reattach it, then press Check disk.", `role="status"`,
		"Check disk", "Disk gone for good? Its volumes are lost. Forget the node under Security.", `hx-post="/-/admin/nodes/`+id+`/disk/check"`)
	if !strings.Contains(section(t, body), "Disk missing") {
		t.Error("the section does not carry the state")
	}
	has(t, "nodes list", s.As(t, root, "GET", "/-/admin/nodes", nil).Body.String(), "Disk missing")

	rec = s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/check", url.Values{})
	has(t, "check", rec.Body.String(), "Disk check queued.")
	for range 200 { // only Check disk clears it
		if v, err := s.Orch.NodeDisk(t.Context(), id); err == nil && v.State == "ready" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Error("Check disk did not bring the disk back")
}

// The controller of an installed panel runs in a container that cannot see
// the host's folders: the section says so and offers nothing; a forged add
// is refused in the dialog.
func TestNodeDiskInstalledPanelControllerIsRefused(t *testing.T) {
	s := webtest.NewWith(t, nil, func(c *service.Config) {
		c.PanelSpec = func(string) service.ContainerSpec { return service.ContainerSpec{} }
		c.Version = "v0.0.1"
	})
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	const why = "The panel cannot use a folder disk yet. Add the disk on a worker node."
	body := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	has(t, "installed", body, why)
	if card := section(t, body); strings.Contains(card, "Add disk") {
		t.Errorf("a button on the installed panel's controller\n%s", card)
	}
	rec := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/probe", url.Values{"path": {"/data"}})
	has(t, "probe", rec.Body.String(), why)
}

// recorder is the disk host with every request noted.
type recorder struct {
	mu    sync.Mutex
	inner interface {
		Disk(context.Context, string, hostdisk.Request) (hostdisk.Result, error)
	}
	got []hostdisk.Request
}

func (r *recorder) Disk(ctx context.Context, node string, q hostdisk.Request) (hostdisk.Result, error) {
	r.mu.Lock()
	r.got = append(r.got, q)
	r.mu.Unlock()
	return r.inner.Disk(ctx, node, q)
}

func (r *recorder) ops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, q := range r.got {
		out = append(out, q.Op)
	}
	return out
}

// The volume drawer says where the volume is and its size from the disk's
// cache. A read never runs a helper of its own: the one cold read starts the
// cache's background walk (a usage op) and answers "unknown"; later reads
// inside the TTL add nothing.
func TestVolumeDrawerReadsSizesFromTheCache(t *testing.T) {
	rec := &recorder{}
	s := webtest.NewWith(t, []service.Option{service.WithDiskHost(rec)})
	rec.inner = service.InProcessDisk(hostdisk.Env{Root: s.Disk.Root, Mountinfo: s.Disk.Mountinfo, TempMounts: true})
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	addedDisk(t, s, root, id, "/data")

	// a volume with a folder on the disk, 5 MiB in it
	vid := s.Volume(t, "env", s.Tile.Env, "pgdata")
	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	mk := reflect.ValueOf(s.Store.DiskFolders.Create)
	f := reflect.New(mk.Type().In(1)).Elem()
	f.FieldByName("ID").SetString("f1")
	f.FieldByName("DiskID").SetString(d.ID)
	f.FieldByName("Name").SetString("pgdata")
	if out := mk.Call([]reflect.Value{reflect.ValueOf(t.Context()), f}); !out[0].IsNil() {
		t.Fatal(out[0].Interface())
	}
	dir := filepath.Join(s.Disk.Dir("/data"), d.ID, "pgdata")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "x"), make([]byte, 5<<20), 0o644))

	get := func() string {
		t.Helper()
		r := s.As(t, root, "GET", "/acme/shop/dev/-/volumes/"+vid, nil)
		if r.Code != 200 {
			t.Fatalf("volume = %d\n%s", r.Code, r.Body)
		}
		return r.Body.String()
	}
	before := len(rec.ops())
	cold := get()
	has(t, "cold", cold, "Stored on", "Node disk", "Ready", "unknown")
	for _, op := range rec.ops()[before:] {
		if op != "usage" {
			t.Errorf("a read ran %q on the host", op)
		}
	}
	var warm string
	for range 200 { // the walk fills the cache; the drawer then has the size
		if warm = get(); strings.Contains(warm, "5.0 MB") {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	has(t, "warm", warm, "5.0 MB", "as of", "/data/"+d.ID+"/pgdata")
	n := len(rec.ops())
	for range 3 {
		get()
	}
	if got := len(rec.ops()); got != n {
		t.Errorf("reads inside the cache's TTL ran the host %d more times: %v", got-n, rec.ops()[n:])
	}

	// the disk goes missing: the banner, the chip, no size and no helper run
	d.State = "missing"
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	n = len(rec.ops())
	gone := get()
	has(t, "missing", gone, "Its disk is missing.", "Disk missing", "Tiles that use it do not start.")
	if got := len(rec.ops()); got != n {
		t.Errorf("a read of a missing disk ran the host: %v", rec.ops()[n:])
	}
}

// A check that finds the disk still gone fails in the mockup's words, and
// the job reads disk.check.
func TestNodeDiskCheckFailsStillMissing(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	addedDisk(t, s, root, id, "/data")
	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	d.State = "missing"
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	s.Disk.Unmount(t, "/data")
	rec := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/check", url.Values{})
	has(t, "check", rec.Body.String(), "disk.check")
	for range 200 {
		js, err := s.Orch.Jobs(t.Context(), 0, "failed")
		must(t, err)
		for _, j := range js {
			if j.Kind == "disk-check" {
				if j.Error != "Still missing. Reattach it, then check again." {
					t.Fatalf("error = %q", j.Error)
				}
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the check never failed")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
