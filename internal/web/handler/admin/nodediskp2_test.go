package admin_test

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/uitest"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Phase 2 screens of the node drawer: Forget disk, Adopt a disk, Move all.

// seedJob records a job nothing picks up (waiting) holding lock.
func seedJob(t *testing.T, s *webtest.Site, id, kind, state string, lock []string, payload string) {
	t.Helper()
	mk := reflect.ValueOf(s.Store.Jobs.Create)
	j := reflect.New(mk.Type().In(1)).Elem()
	j.FieldByName("ID").SetString(id)
	j.FieldByName("Kind").SetString(kind)
	j.FieldByName("State").SetString(state)
	j.FieldByName("LockSet").Set(reflect.ValueOf(lock).Convert(j.FieldByName("LockSet").Type()))
	j.FieldByName("Payload").SetString(payload)
	j.FieldByName("CreatedAt").Set(reflect.ValueOf(time.Now()))
	if out := mk.Call([]reflect.Value{reflect.ValueOf(t.Context()), j}); !out[0].IsNil() {
		t.Fatal(out[0].Interface())
	}
}

// settle waits until no job is queued or running, so a test ends clean.
func settle(t *testing.T, s *webtest.Site) {
	t.Helper()
	for range 400 {
		js, err := s.Orch.Jobs(t.Context(), 0, "queued", "running")
		must(t, err)
		if len(js) == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func noBox(t *testing.T, where, card string) {
	t.Helper()
	uitest.NoPanelInCard(t, "<section>"+card+"</section>")
}

// missingDisk is a ready disk at /data that has gone missing, with the volume
// pgdata (no backup) on it.
func missingDisk(t *testing.T, s *webtest.Site, root, id string) string {
	t.Helper()
	addedDisk(t, s, root, id, "/data")
	s.Volume(t, "env", s.Tile.Env, "pgdata")
	folderRow(t, s, id, "pgdata")
	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	d.State = "missing"
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	return d.ID
}

// 12a, 12c, 12g: a missing disk offers Forget disk (and no Move all) with the
// typed confirm and the volumes it restores; a ready disk offers neither.
func TestForgetDiskIsOfferedOnAMissingDiskOnly(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	diskID := missingDisk(t, s, root, id)
	d, err := s.Store.NodeDisks.Get(t.Context(), diskID)
	must(t, err)
	d.State = "ready"
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	ready := section(t, s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String())
	if strings.Contains(ready, "Forget disk") || strings.Contains(ready, "Forget it.") {
		t.Errorf("a ready disk offers Forget\n%s", ready)
	}
	d.State = "missing"
	must(t, s.Store.NodeDisks.Update(t.Context(), d))

	card := section(t, s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String())
	has(t, "missing", card, "Disk gone for good? Forget it.", ">Forget disk</button>", "Forget this disk",
		"controller, disk "+diskID, `word="controller"`, `data-word`,
		"Its volumes come back from their newest backup. Tiles that use them restart.",
		"Volumes without a backup start empty.", ">pgdata</span>", "starts empty", "shop / dev",
		`<button type="button" class="font-mono text-rw-text cursor-pointer px-1 rounded" data-copy="controller" title="Copy">controller</button>`, `hx-post="/-/admin/nodes/`+id+`/disk/forget"`)
	if strings.Contains(card, "Move all to disk") {
		t.Error("Move all is offered on a missing disk")
	}
	noBox(t, "forget", card)
}

// 12b: the strip of an add that did not finish offers Try again, and the
// section Forget disk.
func TestForgetDiskOfferedOnAHalfAddedDisk(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	addedDisk(t, s, root, id, "/data")
	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	d.State = "adding"
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	body := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	has(t, "adding", body, "Adding did not finish.", "Try again", ">Forget disk</button>", "Forget this disk")
	if strings.Contains(section(t, body), "Move all") {
		t.Error("Move all on a disk that is not ready")
	}
}

// DL: a forget without the typed word changes nothing; with it, the job
// shows as disk.forget and the strip of a forgetting row offers Resume only.
func TestForgetPressNeedsTheWordAndShowsItsJob(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	diskID := missingDisk(t, s, root, id)
	base := "/-/admin/nodes/" + id + "/disk/forget"

	rec := s.As(t, root, "POST", base, url.Values{"disk": {diskID}, "confirm": {"nope"}})
	if rec.Code != 422 {
		t.Errorf("a wrong word = %d\n%s", rec.Code, rec.Body)
	}
	has(t, "wrong word", rec.Body.String(), "type controller to confirm")
	if d, err := s.Store.NodeDisks.Get(t.Context(), diskID); err != nil || d.State != "missing" {
		t.Errorf("a refused forget touched the row: %+v %v", d, err)
	}
	if js, _ := s.Orch.Jobs(t.Context(), 0); len(js) != 0 {
		for _, j := range js {
			if j.Kind == "disk-forget" {
				t.Errorf("a refused forget queued a job: %+v", j)
			}
		}
	}

	rec = s.As(t, root, "POST", base, url.Values{"disk": {diskID}, "confirm": {"controller"}})
	has(t, "forget", rec.Body.String(), "disk.forget", "Forgetting the disk.")
	settle(t, s)

	// a forgetting row with no job: Resume, and the sentence under the facts
	d, err := s.Store.NodeDisks.Get(t.Context(), diskID)
	if err != nil { // the job finished it: nothing to resume
		return
	}
	d.State = "forgetting"
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	body := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	has(t, "forgetting", body, "Forgetting did not finish.", ">Resume</button>", "Forgetting", "Nothing starts on this disk until it is forgotten.")
	if strings.Contains(section(t, body), "Forget disk") {
		t.Error("a forgetting disk offers a second Forget")
	}
}

// forgottenDisk gives the disk row a node that is gone (a forgotten node).
func forgottenDisk(t *testing.T, s *webtest.Site, diskID string) {
	t.Helper()
	d, err := s.Store.NodeDisks.Get(t.Context(), diskID)
	must(t, err)
	d.NodeID = "old"
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	mk := reflect.ValueOf(s.Store.ForgottenNodes.Create)
	f := reflect.New(mk.Type().In(1)).Elem()
	f.FieldByName("ID").SetString("old")
	f.FieldByName("Name").SetString("oldbox")
	f.FieldByName("ForgottenAt").Set(reflect.ValueOf(time.Now()))
	if out := mk.Call([]reflect.Value{reflect.ValueOf(t.Context()), f}); !out[0].IsNil() {
		t.Fatal(out[0].Interface())
	}
}

// 14a, 14b, 12i, 12j: the Adopt entry sits next to Add disk; its first step
// asks for the folder and lists the disks of forgotten nodes, each with its
// Forget record dialog: refused (Close only) while envs point at the node,
// typed and enabled once nothing does.
func TestAdoptEntryStepOneAndForgetRecord(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	body := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	card := section(t, body)
	has(t, "entry", body, "Or adopt a disk from a node that is gone.")
	has(t, "entry", card, ">Adopt a disk</button>", `hx-post="/-/admin/nodes/`+id+`/disk/adopt/plan"`, `id="adopt-dlg"`, ">Add disk</button>")
	noBox(t, "entry", card)

	plan := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/adopt/plan", url.Values{})
	one := plan.Body.String()
	has(t, "step one", one, "Adopt a disk", "Take over a disk whose node is gone. Its volumes stay on it.", `id="adopt-path"`,
		"The mounted folder of a disk from another node.", "Reading the disk...", ">Check</button>", ">Adopt disk</button>")
	if strings.Contains(one, "Disks of forgotten nodes") {
		t.Error("a list of forgotten disks with none")
	}

	// a forgotten node's disk, with an env still pointing at the node
	diskID := missingDisk(t, s, root, id)
	forgottenDisk(t, s, diskID)
	must(t, s.Store.Environments.SetNode(t.Context(), s.Tile.Env, "old"))
	one = s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/adopt/plan", url.Values{}).Body.String()
	has(t, "pointing", one, "Disks of forgotten nodes · 1", diskID, "was oldbox", "Forget record", "Forget this disk record",
		diskID+", was on oldbox", "The record of this disk is removed. Nothing else changes.",
		"still point at oldbox. Recover them first.", `role="alert"`, ">Close</button>")
	if strings.Contains(one, "data-copy") {
		t.Errorf("a refused record asks for a typed word\n%s", one)
	}

	// nothing points at the node any more: the typed confirm, enabled by the word
	must(t, s.Store.Environments.SetNode(t.Context(), s.Tile.Env, id))
	v, err := s.Store.Volumes.ListByNode(t.Context(), "old")
	must(t, err)
	_ = v
	one = s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/adopt/plan", url.Values{}).Body.String()
	has(t, "free", one, "Forget this disk record", `word="`+diskID+`"`, ">Forget record</button>", `data-copy="`+diskID+`"`)

	rec := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/forget", url.Values{"disk": {diskID}, "confirm": {"nope"}})
	if rec.Code != 422 {
		t.Errorf("wrong word = %d", rec.Code)
	}
	if _, err := s.Store.NodeDisks.Get(t.Context(), diskID); err != nil {
		t.Fatalf("a refused record forget removed the row: %v", err)
	}
	rec = s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/forget", url.Values{"disk": {diskID}, "confirm": {diskID}})
	has(t, "gone", rec.Body.String(), "Disk record forgotten.")
	if _, err := s.Store.NodeDisks.Get(t.Context(), diskID); err == nil {
		t.Error("the record stays")
	}
}

// adoptRig is the disk of a forgotten node, mounted at /data on the controller
// (which has no disk of its own): the volume pgdata is on it, legacy is not,
// and the folder old-test belongs to no volume.
func adoptRig(t *testing.T, s *webtest.Site, root, id string) string {
	t.Helper()
	addedDisk(t, s, root, id, "/data")
	s.Volume(t, "env", s.Tile.Env, "pgdata")
	s.Volume(t, "env", s.Tile.Env, "legacy")
	folderRow(t, s, id, "pgdata")
	folderRow(t, s, id, "old-test")
	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	stamp := filepath.Join(s.Disk.Dir("/data"), ".stackr-disk")
	b, err := os.ReadFile(stamp)
	must(t, err)
	m := map[string]any{}
	must(t, json.Unmarshal(b, &m))
	m["owner"] = "old"
	b, _ = json.Marshal(m)
	must(t, os.WriteFile(stamp, b, 0o644))
	forgottenDisk(t, s, d.ID)
	must(t, s.Store.Environments.SetNode(t.Context(), s.Tile.Env, "old"))
	return d.ID
}

var disabledAdopt = regexp.MustCompile(`<button[^>]*\sdisabled(\s[^>]*)?>Adopt disk</button>`)

// 14c: the plan lists the envs that move, the volumes on the disk and not on
// it, the folders kept; the press queues disk.adopt.
func TestAdoptPlanListsWhatMovesAndPressQueuesTheJob(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	diskID := adoptRig(t, s, root, id)
	base := "/-/admin/nodes/" + id + "/disk/adopt"

	body := s.As(t, root, "POST", base+"/plan", url.Values{"path": {"/data"}}).Body.String()
	has(t, "plan", body, "Adopt this disk", "/data, from oldbox (gone)", ">Envs</h3>", "shop / dev", "moves to controller",
		"Volumes on the disk · 1", ">pgdata</span>", "Volumes not on the disk · 1", ">legacy</span>", "starts empty",
		"Unclaimed folders · 1", ">old-test</span>", "kept on the disk", "Unclaimed folders are kept.", ">Adopt disk</button>")
	if disabledAdopt.MatchString(body) {
		t.Errorf("Adopt disk is disabled on a clean plan\n%s", body)
	}
	if strings.Contains(body, "Already on") {
		t.Error("copies listed with none")
	}

	// nothing is adopted without the node's name; a refusal lands in the dialog
	rec := s.As(t, root, "POST", base, url.Values{"disk": {diskID}, "path": {"/data"}, "confirm": {"wrong"}})
	has(t, "wrong", rec.Body.String(), "type controller to confirm", `id="adopt-dlg"`)
	if rec.Header().Get("HX-Retarget") != "#adopt-dlg" {
		t.Errorf("HX-Retarget = %q", rec.Header().Get("HX-Retarget"))
	}
	if d, _ := s.Store.NodeDisks.Get(t.Context(), diskID); d.NodeID != "old" {
		t.Errorf("a refused adopt moved the disk: %+v", d)
	}

	rec = s.As(t, root, "POST", base, url.Values{"disk": {diskID}, "path": {"/data"}, "confirm": {"controller"}})
	has(t, "adopt", rec.Body.String(), "disk.adopt", "Adopting the disk.")
	settle(t, s)
}

// 14d, 14d2: a plain copy in the way disables Adopt disk; Discard opens the
// typed confirm and removes only the Docker object.
func TestAdoptCopyInTheWayIsDiscardedFromTheDialog(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	diskID := adoptRig(t, s, root, id)
	vols := reflect.ValueOf(&s.Docker.Volumes).Elem()
	e := reflect.New(vols.Type().Elem()).Elem()
	e.FieldByName("Name").SetString("pgdata")
	e.FieldByName("SizeBytes").SetInt(9)
	vols.Set(reflect.Append(vols, e))
	base := "/-/admin/nodes/" + id + "/disk/adopt"

	body := s.As(t, root, "POST", base+"/plan", url.Values{"path": {"/data"}}).Body.String()
	has(t, "copy", body, "Already on controller · 1", "copy in Docker storage", ">Discard</button>", "Discard this copy to adopt.",
		"Discard Docker storage copy", "pgdata, shop / dev, on controller", `word="pgdata"`,
		"Deletes the copy in Docker storage. The copy on the node disk stays.", ">Discard copy</button>",
		`hx-post="`+base+`/discard-copy"`, `hx-target="#adopt-dlg"`)
	if !disabledAdopt.MatchString(body) {
		t.Errorf("Adopt disk is enabled with a copy in the way\n%s", body)
	}

	form := url.Values{"disk": {diskID}, "path": {"/data"}, "volume": {volumeOf(t, s, "pgdata")}, "confirm": {"nope"}}
	rec := s.As(t, root, "POST", base+"/discard-copy", form)
	has(t, "wrong word", rec.Body.String(), "type pgdata to remove the plain copy and keep the disk copy")
	for _, c := range s.Docker.Calls() {
		if c.Method == "RemoveVolume" {
			t.Fatalf("a wrong word removed a volume: %s", c)
		}
	}
	form.Set("confirm", "pgdata")
	rec = s.As(t, root, "POST", base+"/discard-copy", form)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="adopt-dlg"`) {
		t.Errorf("discard = %d\n%s", rec.Code, rec.Body)
	}
	removed := false
	for _, c := range s.Docker.Calls() {
		removed = removed || c.Method == "RemoveVolume"
	}
	if !removed {
		t.Error("the copy was not removed")
	}
}

func volumeOf(t *testing.T, s *webtest.Site, slug string) string {
	t.Helper()
	vs, err := s.Orch.Volumes(t.Context(), service.VolumeScope{Kind: "env", ID: s.Tile.Env})
	must(t, err)
	for _, v := range vs {
		if v.Slug == slug {
			return v.ID
		}
	}
	t.Fatalf("no volume %s", slug)
	return ""
}

// 14f, 14g, 14i: refusals read under the folder field, in the dialog's words.
func TestAdoptRefusalsReadInTheDialogsWords(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	diskID := adoptRig(t, s, root, id)
	base := "/-/admin/nodes/" + id + "/disk/adopt/plan"
	plan := func() string {
		return s.As(t, root, "POST", base, url.Values{"path": {"/data"}}).Body.String()
	}

	// the old node is alive: online, then offline but not forgotten
	w := s.Worker(t, "vm-hel-1", "x")
	d, err := s.Store.NodeDisks.Get(t.Context(), diskID)
	must(t, err)
	d.NodeID = w
	must(t, s.Store.NodeDisks.Update(t.Context(), d))
	has(t, "online", plan(), "vm-hel-1 is still online. Forget the node first.", `aria-invalid="true"`)
	n, err := s.Store.Nodes.Get(t.Context(), w)
	must(t, err)
	n.Status = "offline"
	must(t, s.Store.Nodes.Update(t.Context(), n))
	one := plan()
	has(t, "offline", one, "vm-hel-1 is offline, not forgotten. Forget the node first.")
	if disabledAdopt.MatchString(one) == false {
		t.Error("Adopt disk enabled on a refusal")
	}

	// a folder with no stamp of a disk this panel knows
	s.Disk.Mount(t, "/blank")
	one = s.As(t, root, "POST", base, url.Values{"path": {"/blank"}}).Body.String()
	if !strings.Contains(one, "This folder is not a stackr disk.") && !strings.Contains(one, "role=\"alert\"") {
		t.Errorf("an unstamped folder is not refused\n%s", one)
	}
}

// 16a-16e: Move all shows how many volumes are still in Docker storage, the
// plan groups them with what restarts, the press queues one job per group, and
// the button is off while a move runs, with each job in the live region.
func TestMoveAllScreens(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	addedDisk(t, s, root, id, "/data")
	vid := s.Volume(t, "env", s.Tile.Env, "uploads")
	base := "/-/admin/nodes/" + id + "/disk/move-all"

	card := section(t, s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String())
	has(t, "16a", card, "1 volume is still in Docker storage.", ">Move all to disk</button>", `hx-post="`+base+`/plan"`, `id="moveall-dlg"`)
	noBox(t, "16a", card)

	// the rig's controller reports no disk facts, so the plan is refused in the dialog
	plan := s.As(t, root, "POST", base+"/plan", url.Values{}).Body.String()
	has(t, "16b", plan, "Move all to disk", "0 volumes on controller", "The node has not read its disk yet.", `role="alert"`)

	// a move is running: the button is off, the job has its line
	seedJob(t, s, "j-move", "volume-to-disk", "waiting", []string{"todisk:" + id},
		`{"node_id":"`+id+`","volumes":[{"volume_id":"`+vid+`"}]}`)
	body := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	card = section(t, body)
	has(t, "16d", card, "A move is running.")
	if !regexp.MustCompile(`<button[^>]*\sdisabled(\s[^>]*)?>Move all to disk</button>`).MatchString(card) {
		t.Errorf("Move all is on while a move runs\n%s", card)
	}
	has(t, "16e", body, "volume.to_disk", ">uploads</span>", ">queued<")
	noBox(t, "16e", card)

	// a press the plan refuses is answered in the dialog, which Retargets
	rec := s.As(t, root, "POST", base, url.Values{})
	if rec.Header().Get("HX-Retarget") != "#moveall-dlg" {
		t.Errorf("a refused press: %d, HX-Retarget %q\n%s", rec.Code, rec.Header().Get("HX-Retarget"), rec.Body)
	}
	has(t, "refused", rec.Body.String(), "The node has not read its disk yet.", "Move all to disk")
}

// A press the plan refuses queues nothing and answers in the dialog.
func TestMoveAllPressRefusedStaysInTheDialog(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	addedDisk(t, s, root, id, "/data")
	s.Volume(t, "env", s.Tile.Env, "uploads")
	rec := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/move-all", url.Values{})
	has(t, "press", rec.Body.String(), "The node has not read its disk yet.")
	if rec.Header().Get("HX-Retarget") != "#moveall-dlg" || rec.Header().Get("HX-Reswap") != "innerHTML" {
		t.Errorf("headers: %v", rec.Header())
	}
	js, err := s.Orch.Jobs(t.Context(), 0)
	must(t, err)
	for _, j := range js {
		if j.Kind == "volume-to-disk" {
			t.Errorf("a refused press queued %+v", j)
		}
	}
}

// Every new GET and poll is rows and memory: no helper run, no Docker call
// about a volume, in each state of the section.
func TestDiskScreensPollNeverReachDockerOrAHelper(t *testing.T) {
	rec := &recorder{}
	s := webtest.NewWith(t, []service.Option{service.WithDiskHost(rec)})
	rec.inner = service.InProcessDisk(hostdisk.Env{Root: s.Disk.Root, Mountinfo: s.Disk.Mountinfo, TempMounts: true})
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	addedDisk(t, s, root, id, "/data")
	s.Volume(t, "env", s.Tile.Env, "uploads")
	get := func(where string) {
		t.Helper()
		n, h := len(s.Docker.Calls()), len(rec.ops())
		if r := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil); r.Code != 200 {
			t.Fatalf("%s = %d", where, r.Code)
		}
		for _, c := range s.Docker.Calls()[n:] {
			switch c.Method {
			case "InspectVolume", "VolumeDevice", "ListVolumes", "DiskUsage", "ContainerList", "CreateVolume", "RemoveVolume":
				t.Errorf("%s: a poll asked Docker: %s", where, c)
			}
		}
		for _, op := range rec.ops()[h:] {
			if op != "usage" { // the cache's own background walk
				t.Errorf("%s: a poll ran %q on the host", where, op)
			}
		}
	}
	get("ready, a volume in Docker")
	seedJob(t, s, "j-m", "volume-to-disk", "waiting", []string{"todisk:" + id}, `{"node_id":"`+id+`","volumes":[]}`)
	get("a move running")
	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	for _, st := range []string{"missing", "adding", "forgetting", "adopting"} {
		d.State = st
		must(t, s.Store.NodeDisks.Update(t.Context(), d))
		get(st)
	}
}
