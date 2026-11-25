package admin_test

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Phase 3 of the node drawer: the whole-disk Add disk dialog and Use it. The
// helper runs only in the choice of a whole disk and its Refresh; a pick,
// Continue and every read run none, and a format never goes out without the
// typed name.

// devHost is a node whose only disks are devs. It records every op.
type devHost struct {
	mu   sync.Mutex
	got  []hostdisk.Request
	devs []hostdisk.Device
	err  error
	// inner answers every op but list: the rig's real helper, for a test that
	// adds a folder disk first.
	inner interface {
		Disk(context.Context, string, hostdisk.Request) (hostdisk.Result, error)
	}
}

func (h *devHost) Disk(_ context.Context, _ string, r hostdisk.Request) (hostdisk.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, r)
	if h.err != nil {
		return hostdisk.Result{}, h.err
	}
	if r.Op == "list" {
		return hostdisk.Result{Devices: h.devs}, nil
	}
	if h.inner != nil {
		return h.inner.Disk(context.Background(), "", r)
	}
	return hostdisk.Result{}, nil
}

func (h *devHost) ops() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.got {
		out = append(out, r.Op)
	}
	return out
}

var (
	devEmpty = hostdisk.Device{Path: "/dev/sdb", ID: "ata-Samsung_1", Model: "Samsung SSD", Serial: "S5Y1", Bytes: 500 << 30}
	devExt4  = hostdisk.Device{Path: "/dev/sdc", ID: "ata-WDC_2", Model: "WDC", Bytes: 2 << 40, FSType: "ext4", Label: "backup", Signature: "filesystem"}
	devMnt   = hostdisk.Device{Path: "/dev/sda", ID: "ata-Sys_0", Bytes: 80 << 30, Blocked: "system"}
)

func devSite(t *testing.T) (*webtest.Site, *devHost, string, string) {
	t.Helper()
	h := &devHost{devs: []hostdisk.Device{devMnt, devExt4, devEmpty}}
	s := webtest.NewWith(t, []service.Option{service.WithDiskHost(h)})
	h.inner = service.InProcessDisk(hostdisk.Env{Root: s.Disk.Root, Mountinfo: s.Disk.Mountinfo, TempMounts: true})
	root := s.Session(t, s.User(t, "root@x.test", true))
	return s, h, root, diskNode(t, s)
}

func devJSON(t *testing.T, d hostdisk.Device) string {
	t.Helper()
	b, err := json.Marshal(d)
	must(t, err)
	return string(b)
}

func formatJobs(t *testing.T, s *webtest.Site) []service.Job {
	t.Helper()
	js, err := s.Orch.Jobs(t.Context(), 0)
	must(t, err)
	return slices.DeleteFunc(js, func(j service.Job) bool { return j.Kind != "disk-add-device" })
}

// 17a to 17f: the folder choice runs no helper, the whole disk choice lists
// the disks once (usable first, blocked greyed with the reason), Refresh lists
// again, and a pick or Continue never reaches the helper.
func TestWholeDiskChoiceListsOnTheDiskChoiceOnly(t *testing.T) {
	s, h, root, id := devSite(t)
	base := "/-/admin/nodes/" + id + "/disk/"

	rec := s.As(t, root, "POST", base+"choose", url.Values{"how": {"folder"}})
	has(t, "folder", rec.Body.String(), `id="disk-path"`, ">Use folder</button>")
	if len(h.ops()) != 0 {
		t.Fatalf("the folder choice ran %v", h.ops())
	}

	rec = s.As(t, root, "POST", base+"choose", url.Values{"how": {"disk"}})
	body := rec.Body.String()
	has(t, "disk", body, "Disks on ", "Samsung SSD", "500 GB", "S5Y1", "No filesystem", "ext4 filesystem", "Not available: it holds the system or Docker.",
		`hx-swap-oob="innerHTML:#disk-foot"`, `id="disk-continue"`)
	if !slices.Equal(h.ops(), []string{"list"}) {
		t.Fatalf("the choice ran %v, want one list", h.ops())
	}
	if sys := strings.Index(body, "ata-Sys_0"); sys >= 0 && strings.Index(body, "Samsung") > sys {
		t.Error("a blocked disk is listed before a usable one")
	}
	s.As(t, root, "POST", base+"choose", url.Values{"how": {"disk"}, "device": {devJSON(t, devEmpty)}})
	if !slices.Equal(h.ops(), []string{"list", "list"}) {
		t.Errorf("Refresh ran %v", h.ops())
	}

	// a pick moves Continue; Continue draws the dialog; neither runs the helper
	before := len(h.ops())
	pick := s.As(t, root, "POST", base+"pick", url.Values{"device": {devJSON(t, devEmpty)}}).Body.String()
	if !strings.Contains(pick, `hx-swap-oob="innerHTML:#disk-foot"`) || !strings.Contains(pick, `id="disk-continue"`) || strings.Contains(pick, " disabled>") {
		t.Errorf("a usable pick leaves Continue off\n%s", pick)
	}
	if blocked := s.As(t, root, "POST", base+"pick", url.Values{"device": {devJSON(t, devMnt)}}).Body.String(); !strings.Contains(blocked, " disabled>") {
		t.Errorf("a blocked pick enables Continue\n%s", blocked)
	}
	plan := s.As(t, root, "POST", base+"format/plan", url.Values{"device": {devJSON(t, devEmpty)}}).Body.String()
	has(t, "plan", plan, "Format sdb", "Erases sdb and formats it xfs.", `word="sdb"`, ">Format disk</button>", "S5Y1", "ata-Samsung_1")
	if strings.Contains(plan, "force") {
		t.Error("a plain format posts force")
	}
	if len(h.ops()) != before {
		t.Errorf("a pick or Continue ran %v", h.ops()[before:])
	}
}

// 17i, 17k2, 17n: a disk that holds data is only offered Format anyway; a
// disk that cannot be formatted has no format at all.
func TestFormatPlanSaysWhatTheDiskHolds(t *testing.T) {
	s, _, root, id := devSite(t)
	plan := func(d hostdisk.Device) string {
		return s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/format/plan", url.Values{"device": {devJSON(t, d)}}).Body.String()
	}
	out := plan(devExt4)
	has(t, "17i", out, "Format sdc anyway", "sdc holds data. Formatting erases all of it. It cannot be undone.", "ext4 filesystem, label backup", ">Format anyway</button>", "&#34;force&#34;:&#34;1&#34;")
	tbl := devEmpty
	tbl.Signature, tbl.PTType = "table", "gpt"
	has(t, "17k2", plan(tbl), "sdb has a partition table.", "an empty partition table (gpt)")
	stk := devEmpty
	stk.Signature = "stackr"
	has(t, "17k", plan(stk), "An earlier stackr disk. Its volumes are erased.", "To keep them, adopt the disk instead.")
	out = plan(devMnt)
	has(t, "17n", out, "That disk cannot be used: it holds the system or Docker.", ">Close</button>")
	if strings.Contains(out, "data-word") || strings.Contains(out, `hx-post=`) {
		t.Errorf("a blocked disk can be formatted\n%s", out)
	}
	if rec := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/format/plan", url.Values{"device": {"not json"}}); rec.Code != 400 {
		t.Errorf("no disk picked = %d", rec.Code)
	}
}

// DL: a format is queued only with the typed name; the wrong word, no word and
// a blocked disk queue nothing and reach no helper.
func TestFormatNeedsTheTypedName(t *testing.T) {
	s, h, root, id := devSite(t)
	post := func(f url.Values) (int, string) {
		rec := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/format", f)
		return rec.Code, rec.Body.String()
	}
	for name, typed := range map[string]string{"no word": "", "wrong word": "sdc", "partial": "sd"} {
		_, body := post(url.Values{"device": {devJSON(t, devEmpty)}, "typed": {typed}})
		has(t, name, body, "type sdb to confirm", ">Close</button>")
	}
	_, body := post(url.Values{"device": {devJSON(t, devMnt)}, "typed": {"sda"}})
	has(t, "blocked", body, "That disk cannot be used: it holds the system or Docker.")
	if js := formatJobs(t, s); len(js) != 0 || len(h.ops()) != 0 {
		t.Fatalf("a refused format queued %v and ran %v", js, h.ops())
	}

	// the name, the path or the id each confirm; the answer is the drawer with the job
	for _, typed := range []string{"sdb"} {
		code, body := post(url.Values{"device": {devJSON(t, devEmpty)}, "typed": {typed}})
		if code != 200 {
			t.Fatalf("typed %q = %d\n%s", typed, code, body)
		}
		has(t, "queued", body, "Formatting sdb.", "disk.add_device")
		// the note goes with its job: the drawer reloads when the job ends
		has(t, "reloads", body, `hx-get="/-/admin/nodes/`+id+`?tab=overview" hx-trigger="sse:end"`)
	}
	js := formatJobs(t, s)
	if len(js) != 1 {
		t.Fatalf("jobs = %v", js)
	}
	var p struct {
		Device *hostdisk.Device `json:"device"`
		Force  bool             `json:"force"`
	}
	must(t, json.Unmarshal([]byte(js[0].Payload), &p))
	if p.Device == nil || p.Device.ID != devEmpty.ID || p.Force {
		t.Errorf("payload = %s, want the whole listed disk without force", js[0].Payload)
	}
	settle(t, s)
}

// DL: force reaches the job only from Format anyway, and only for a disk that
// holds something. A plain format of such a disk is refused with the words.
func TestForceOnlyViaFormatAnyway(t *testing.T) {
	s, _, root, id := devSite(t)
	post := func(d hostdisk.Device, typed, force string) string {
		f := url.Values{"device": {devJSON(t, d)}, "typed": {typed}}
		if force != "" {
			f.Set("force", force)
		}
		return s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/format", f).Body.String()
	}
	body := post(devExt4, "sdc", "")
	has(t, "no force", body, "That disk already holds data. Use Format anyway to erase it.", ">Close</button>", "Format sdc anyway")
	if len(formatJobs(t, s)) != 0 {
		t.Fatal("a disk that holds data was formatted without Format anyway")
	}
	// an empty disk with force posted: the job does not carry it
	post(devEmpty, "sdb", "1")
	js := formatJobs(t, s)
	if len(js) != 1 || strings.Contains(js[0].Payload, `"force":true`) {
		t.Fatalf("force reached the job of an empty disk: %v", js)
	}
	settle(t, s)
}

// 17o, 17p: refusals of the service land in the dialog's own words.
func TestFormatRefusalsLandInTheDialog(t *testing.T) {
	s, _, root, id := devSite(t)
	post := func() string {
		return s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/format", url.Values{"device": {devJSON(t, devEmpty)}, "typed": {"sdb"}}).Body.String()
	}
	// a node that already has its disk
	addedDisk(t, s, root, id, "/data")
	body := post()
	has(t, "17o", body, "This node already has a disk.", ">Close</button>")
	if len(formatJobs(t, s)) != 0 {
		t.Error("a node with a disk queued a format")
	}
}

// A silent node and a failed read answer inside the picker.
func TestWholeDiskChoiceErrors(t *testing.T) {
	h := &devHost{err: context.DeadlineExceeded}
	s := webtest.NewWith(t, []service.Option{service.WithDiskHost(h)})
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	body := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/choose", url.Values{"how": {"disk"}}).Body.String()
	has(t, "silent", body, "did not answer. Try again.")
	if strings.Contains(body, "No disk can be used") {
		t.Error("a failed read says no disk can be used")
	}
}

// Try again of an xfs add that stopped opens the whole dialog, open, on the
// picker; the folder choice answers only the part.
func TestRetryOpensThePicker(t *testing.T) {
	s, _, root, id := devSite(t)
	body := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/choose", url.Values{"how": {"disk"}, "open": {"1"}}).Body.String()
	has(t, "retry", body, "<confirm-dialog open", "Disks on ", `id="disk-fmt-dlg"`, "A whole disk")
	if strings.Contains(body, "data-open") {
		t.Error("the opened dialog has an opener")
	}
}

// xfsDisk makes the node's disk (added as a folder) an xfs disk by its row,
// the way a worker-less rig can have one.
func xfsDisk(t *testing.T, s *webtest.Site, root, id, state, fsuuid string) {
	t.Helper()
	addedDisk(t, s, root, id, "/data")
	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	d.Kind, d.DeviceID, d.FSUUID, d.DeviceBytes, d.State = "xfs", "ata-Samsung_1", fsuuid, 500<<30, state
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
}

// 17q to 17t, 18a: the drawer is a read. It runs no helper, shows the device,
// the adding steps and the strip of a stopped add, and a ready disk offers no
// Use it without new space.
func TestXfsNodeDrawerIsAReadAndSaysTheSteps(t *testing.T) {
	s, h, root, id := devSite(t)
	xfsDisk(t, s, root, id, "ready", "u1")
	before := len(h.ops())
	body := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	card := section(t, body)
	has(t, "ready", card, ">Device</dt>", "ata-Samsung_1", "500 GB", "Ready", "Volumes on this disk")
	if strings.Contains(card, "Use it") || strings.Contains(card, "New space") {
		t.Errorf("Use it with no new space\n%s", card)
	}
	noBox(t, "17t", card)

	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	d.State = "adding"
	d.FSUUID = ""
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	body = s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	has(t, "17s2", body, "Adding did not finish.", ">Try again</button>", `hx-post="/-/admin/nodes/`+id+`/disk/choose"`, `id="disk-retry"`, ">Forget disk</button>")
	if strings.Contains(body, ">Check disk<") || strings.Contains(body, "mounts it") {
		t.Errorf("17s2: a disk nothing was formatted on offers Check disk or the mount note\n%s", body)
	}

	d.FSUUID = "u1"
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	body = s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	has(t, "17s", body, "Adding did not finish.", ">Check disk</button>", ">Try again</button>", "Try again mounts it. It is not formatted a second time.")

	// a queued add of either kind says what it does and offers no retry
	seedJob(t, s, "j-dev", "disk-add-device", "waiting", nil, `{"node_id":"`+id+`"}`)
	body = s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	has(t, "17r", section(t, body), "Mounting ata-Samsung_1.")
	if strings.Contains(body, "Adding did not finish.") {
		t.Error("a running add shows the stopped strip")
	}
	d.FSUUID = ""
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	has(t, "17q", section(t, s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()), "Formatting ata-Samsung_1.")

	if got := h.ops()[before:]; slices.Contains(got, "list") || slices.Contains(got, "format") {
		t.Errorf("a GET ran %v on the host", got)
	}
}

// 18b: Use it queues the grow job and shows it; only an xfs disk grows.
func TestGrowQueuesTheJobAndRefusesAPathDisk(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	addedDisk(t, s, root, id, "/data")
	rec := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/grow", nil)
	if rec.Code != 422 {
		t.Errorf("grow of a folder disk = %d", rec.Code)
	}
	has(t, "path", rec.Body.String(), "Only a disk made from a whole device can grow.")
	if js, _ := s.Orch.Jobs(t.Context(), 0); slices.ContainsFunc(js, func(j service.Job) bool { return j.Kind == "disk-grow" }) {
		t.Error("a folder disk queued a grow")
	}

	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	d.Kind, d.DeviceID, d.FSUUID, d.DeviceBytes = "xfs", "ata-Samsung_1", "u1", 500<<30
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	rec = s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/grow", nil)
	if rec.Code != 200 {
		t.Fatalf("grow = %d\n%s", rec.Code, rec.Body)
	}
	has(t, "grow", rec.Body.String(), "Growing the disk.", "disk.grow")
	settle(t, s)
}

// Only a server admin reaches the new routes.
func TestWholeDiskRoutesAreServerAdminOnly(t *testing.T) {
	s, h, _, id := devSite(t)
	member := s.Session(t, s.User(t, "member@x.test", false))
	for _, op := range []string{"choose", "pick", "format/plan", "format", "grow"} {
		rec := s.As(t, member, "POST", "/-/admin/nodes/"+id+"/disk/"+op, url.Values{"how": {"disk"}, "device": {devJSON(t, devEmpty)}, "typed": {"sdb"}})
		if rec.Code != 403 && rec.Code != 404 {
			t.Errorf("%s as a member = %d", op, rec.Code)
		}
	}
	if len(h.ops()) != 0 || len(formatJobs(t, s)) != 0 {
		t.Errorf("a member reached the helper %v or queued a format", h.ops())
	}
}
