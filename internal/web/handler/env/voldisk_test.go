package env_test

import (
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/ui/uitest"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Phase 2 screens of the volume drawer: Move to disk, the two-copies banner
// and its Discard, the held old copy.

func mustf(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func hasAll(t *testing.T, where, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("%s: no %q in\n%.3000s", where, w, body)
		}
	}
}

// disk gives the controller a ready disk at /data and returns the node and disk ids.
func disk(t *testing.T, s *webtest.Site, root string) (node, diskID string) {
	t.Helper()
	ns, err := s.Orch.Nodes(t.Context())
	if err != nil || len(ns) != 1 {
		t.Fatalf("nodes = %+v, %v", ns, err)
	}
	node = ns[0].ID
	s.Disk.Mount(t, "/data")
	base := "/-/admin/nodes/" + node + "/disk/"
	s.As(t, root, "POST", base+"probe", url.Values{"path": {"/data"}})
	if rec := s.As(t, root, "POST", base+"add", url.Values{"path": {"/data"}, "checked": {"/data"}}); rec.Code != 200 {
		t.Fatalf("add = %d\n%s", rec.Code, rec.Body)
	}
	for range 200 {
		if v, err := s.Orch.NodeDisk(t.Context(), node); err == nil && v.State == "ready" {
			d, err := s.Store.NodeDisks.GetByNode(t.Context(), node)
			mustf(t, err)
			return node, d.ID
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the disk never went ready")
	return "", ""
}

func create(t *testing.T, create any, set map[string]any) {
	t.Helper()
	mk := reflect.ValueOf(create)
	v := reflect.New(mk.Type().In(1)).Elem()
	for k, x := range set {
		f := v.FieldByName(k)
		xv := reflect.ValueOf(x)
		if xv.Type() != f.Type() {
			xv = xv.Convert(f.Type())
		}
		f.Set(xv)
	}
	if out := mk.Call([]reflect.Value{reflect.ValueOf(t.Context()), v}); !out[0].IsNil() {
		t.Fatal(out[0].Interface())
	}
}

// folder gives the disk a folder row (and the folder) for name.
func folder(t *testing.T, s *webtest.Site, diskID, name string) {
	t.Helper()
	create(t, s.Store.DiskFolders.Create, map[string]any{"ID": "f-" + name, "DiskID": diskID, "Name": name})
	mustf(t, os.MkdirAll(filepath.Join(s.Disk.Dir("/data"), diskID, name), 0o755))
}

func seedJob(t *testing.T, s *webtest.Site, id, kind, state string, lock []string, payload string) {
	t.Helper()
	create(t, s.Store.Jobs.Create, map[string]any{"ID": id, "Kind": kind, "State": state, "LockSet": lock, "Payload": payload, "CreatedAt": time.Now()})
}

// storedOn is the Stored on card of an overview.
func storedOn(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, ">Stored on</p>")
	if !ok {
		t.Fatalf("no Stored on card in\n%.2000s", body)
	}
	card, _, _ := strings.Cut(rest, `<div class="panel p-4`)
	return card
}

func noBoxIn(t *testing.T, where, card string) {
	t.Helper()
	uitest.NoPanelInCard(t, "<section>"+card+"</section>")
}

// 15a, 15f, 15g: the button sits in Stored on of a volume in Docker storage;
// a disk that is not ready disables it with a hint; a live move disables it.
func TestVolumeMoveButtonStates(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	node, diskID := disk(t, s, root)
	vid := s.Volume(t, "env", s.Tile.Env, "uploads")
	get := func() string {
		t.Helper()
		r := s.As(t, root, "GET", "/acme/shop/dev/-/volumes/"+vid, nil)
		if r.Code != 200 {
			t.Fatalf("drawer = %d", r.Code)
		}
		return r.Body.String()
	}
	body := get()
	card := storedOn(t, body)
	// this rig reports no disk facts, so the room cannot be read: the button is off with the plan's reason
	hasAll(t, "15a", card, "Docker storage", " disabled>Move to disk</button>", "The node has not read its disk yet.")
	if strings.Contains(card, "hx-post") {
		t.Errorf("a disabled Move to disk posts\n%s", card)
	}
	noBoxIn(t, "15a", card)
	if strings.Contains(body, "Finish move") {
		t.Error("Finish move on a volume that never moved")
	}

	// a live move: disabled, no hint, its job in the slot
	seedJob(t, s, "j1", "volume-to-disk", "waiting", []string{"todisk:" + node, "volume:" + vid},
		`{"node_id":"`+node+`","volumes":[{"volume_id":"`+vid+`"}]}`)
	body = get()
	hasAll(t, "15g", body, `id="job-slot"`, "volume.to_disk")
	card = storedOn(t, body)
	if !strings.Contains(card, " disabled>Move to disk</button>") && !strings.Contains(card, " disabled ") {
		t.Errorf("Move to disk is on while a move runs\n%s", card)
	}
	mustf(t, s.Store.Jobs.Delete(t.Context(), "j1"))

	// the disk is not ready: disabled, with the reason
	d, err := s.Store.NodeDisks.Get(t.Context(), diskID)
	mustf(t, err)
	d.State = "missing"
	mustf(t, s.Store.NodeDisks.Update(t.Context(), d))
	card = storedOn(t, get())
	hasAll(t, "15f", card, "The node disk is not ready.", "Move to disk")
	if strings.Contains(card, "hx-post") {
		t.Errorf("a disabled Move to disk posts\n%s", card)
	}
}

// 15b-15d, 15k: the plan POST opens the dialog; in this rig the node reports
// no disk facts, so the room cannot be read and the dialog says so with the
// button off. A press that is refused answers in the dialog, in its words.
func TestVolumeMoveDialogAndRefusals(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	node, _ := disk(t, s, root)
	vid := s.Volume(t, "env", s.Tile.Env, "uploads")
	base := "/acme/shop/dev/-/volumes/" + vid + "/move"

	rec := s.As(t, root, "POST", base+"/plan", url.Values{})
	hasAll(t, "plan", rec.Body.String(), "<confirm-dialog open>", "Move uploads to the node disk", "uploads, shop / dev", "The node has not read its disk yet.", `role="alert"`)

	// a backup holds the volume: the dialog's words, retargeted to the dialog
	seedJob(t, s, "j-bak", "backup", "running", []string{"volume:" + vid}, `{}`)
	rec = s.As(t, root, "POST", base, url.Values{})
	hasAll(t, "backup", rec.Body.String(), "A backup is running. Try again when it is done.", `role="alert"`)
	if rec.Header().Get("HX-Retarget") != "#vol-move-dlg" || rec.Header().Get("HX-Reswap") != "innerHTML" {
		t.Errorf("a refused press: %d %v", rec.Code, rec.Header())
	}
	mustf(t, s.Store.Jobs.Delete(t.Context(), "j-bak"))
	js, err := s.Orch.Jobs(t.Context(), 0)
	mustf(t, err)
	for _, j := range js {
		if j.Kind == "volume-to-disk" {
			t.Errorf("a refused press queued %+v", j)
		}
	}
	_ = node
}

// 15j: a move cut off after the swap shows the strip with Finish move and no
// button in the card; the last job reads volume.to_disk, failed.
func TestVolumeMoveFinishStrip(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	node, diskID := disk(t, s, root)
	vid := "11111111-2222-3333-4444-555555555555"
	name := "stackr-vol-" + vid + "-disk"
	create(t, s.Store.Volumes.Create, map[string]any{"ID": vid, "ScopeKind": "env", "ScopeID": s.Tile.Env, "Slug": "uploads", "Name": name, "CreatedAt": time.Now()})
	folder(t, s, diskID, name)
	seedJob(t, s, "j-f", "volume-to-disk", "failed", []string{}, `{"node_id":"`+node+`","volumes":[{"volume_id":"`+vid+`"}]}`)
	body := s.As(t, root, "GET", "/acme/shop/dev/-/volumes/"+vid, nil).Body.String()
	hasAll(t, "15j", body, "Move did not finish.", ">Finish move</button>", `hx-post="/acme/shop/dev/-/volumes/`+vid+`/move"`, "volume.to_disk", "failed", `role="status"`)
	if strings.Contains(storedOn(t, body), ">Move to disk<") {
		t.Error("Move to disk beside Finish move")
	}
}

// ghostRig is a volume pgdata with a folder on the disk, and a Docker object
// of the same name that should not be there.
func ghostRig(t *testing.T, s *webtest.Site, root string, plain bool) (vid, diskID string) {
	t.Helper()
	_, diskID = disk(t, s, root)
	vid = s.Volume(t, "env", s.Tile.Env, "pgdata")
	folder(t, s, diskID, "pgdata")
	if plain {
		vols := reflect.ValueOf(&s.Docker.Volumes).Elem()
		e := reflect.New(vols.Type().Elem()).Elem()
		e.FieldByName("Name").SetString("pgdata")
		e.FieldByName("SizeBytes").SetInt(9)
		vols.Set(reflect.Append(vols, e))
		mustf(t, s.Docker.CreateVolume(t.Context(), "pgdata", "local", nil, nil))
	} else {
		mustf(t, s.Docker.CreateVolume(t.Context(), "pgdata", "local", map[string]string{"type": "none", "o": "bind", "device": "/elsewhere/pgdata"}, nil))
	}
	return vid, diskID
}

func removed(s *webtest.Site) bool {
	for _, c := range s.Docker.Calls() {
		if c.Method == "RemoveVolume" {
			return true
		}
	}
	return false
}

// 13a-13c: two copies. The banner appears once a gate has found the plain copy,
// carries Discard Docker storage copy with its typed confirm, and the verb
// removes only the Docker object once the volume's name is typed.
func TestVolumeGhostTwoCopies(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	vid, _ := ghostRig(t, s, root, true)
	url := "/acme/shop/dev/-/volumes/" + vid

	if body := s.As(t, root, "GET", url, nil).Body.String(); strings.Contains(body, "Two copies") {
		t.Fatal("a banner before any gate looked")
	}
	// a press without the word reaches the gate, finds the copy and refuses
	rec := s.As(t, root, "POST", url+"/discard-copy", nil)
	if rec.Code != 422 || removed(s) {
		t.Fatalf("no word = %d, removed %v\n%.600s", rec.Code, removed(s), rec.Body)
	}
	body := s.As(t, root, "GET", url, nil).Body.String()
	hasAll(t, "13a", body, "Two copies of this volume.", "One on the node disk, one in Docker storage. Deploys, backups and delete are refused.",
		`class="banner banner-danger"`, ">Discard Docker storage copy</button>", "Discard Docker storage copy", "pgdata, shop / dev",
		`word="pgdata"`, "Deletes the copy in Docker storage. The copy on the node disk stays.", ">Discard copy</button>",
		`hx-post="`+url+`/discard-copy"`, "Node disk", "Docker storage")

	rec = s.As(t, root, "POST", url+"/discard-copy", map[string][]string{"confirm": {"pgdata"}})
	if rec.Code != 200 || !removed(s) {
		t.Fatalf("typed = %d, removed %v\n%.600s", rec.Code, removed(s), rec.Body)
	}
	if body := s.As(t, root, "GET", url, nil).Body.String(); strings.Contains(body, "Two copies") {
		t.Error("the banner stays after the copy is gone")
	}
}

// 13d-13f: an object bound to the wrong folder: Mismatch, Remove link, both
// paths in the confirm (for a node admin).
func TestVolumeGhostWrongFolder(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	vid, diskID := ghostRig(t, s, root, false)
	url := "/acme/shop/dev/-/volumes/" + vid
	s.As(t, root, "POST", url+"/discard-object", nil) // a gate looks
	body := s.As(t, root, "GET", url, nil).Body.String()
	hasAll(t, "13d", body, "This volume points at the wrong folder.", "Deploys, backups and delete are refused.", ">Remove link</button>",
		"Mismatch", "/elsewhere/pgdata", "Remove the wrong link", "Removes the link only. Both folders keep their data.",
		"Points at", "Should be", "/data/"+diskID+"/pgdata", `word="pgdata"`, `hx-post="`+url+`/discard-object"`)
	noBoxIn(t, "13d", storedOn(t, body))
	if strings.Contains(body, "Two copies") {
		t.Error("the wrong-folder state reads as two copies")
	}
	s.As(t, root, "POST", url+"/discard-object", map[string][]string{"confirm": {"pgdata"}})
	if !removed(s) {
		t.Error("the object was not removed")
	}
}

// A held old copy (orphaned_at = the move's hold) shows no date, a plain
// orphan keeps its date.
func TestVolumeOldCopyShowsNoDate(t *testing.T) {
	s := webtest.New(t)
	hold := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id, slug string, at time.Time) {
		create(t, s.Store.Volumes.Create, map[string]any{"ID": id, "ScopeKind": "env", "ScopeID": s.Tile.Env, "Slug": slug, "Name": slug, "CreatedAt": time.Now(), "OrphanedAt": &at})
	}
	mk("v-old", "old", hold)
	mk("v-orph", "orph", time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC))
	old := s.Do(t, "GET", "/acme/shop/dev/-/volumes/v-old", nil).Body.String()
	hasAll(t, "old copy", old, "Old copy, kept until the move finishes.")
	if strings.Contains(old, "Removed on") || strings.Contains(old, "9999") || strings.Contains(old, "Restore") {
		t.Errorf("the hold shows as a date\n%.2000s", old)
	}
	hasAll(t, "orphan", s.Do(t, "GET", "/acme/shop/dev/-/volumes/v-orph", nil).Body.String(), "Removed on 02-07. Its data is deleted on 01-08.", "Restore", "/volumes/v-orph/restore-volume")
}

// The drawer polls: none of these states asks Docker about the volume.
func TestVolumeDiskScreensPollNeverAskDocker(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	vid, _ := ghostRig(t, s, root, true)
	url := "/acme/shop/dev/-/volumes/" + vid
	s.As(t, root, "POST", url+"/discard-copy", nil)
	n := len(s.Docker.Calls())
	for range 2 {
		if r := s.As(t, root, "GET", url, nil); r.Code != 200 || !strings.Contains(r.Body.String(), "Two copies") {
			t.Fatalf("poll = %d", r.Code)
		}
	}
	for _, c := range s.Docker.Calls()[n:] {
		switch c.Method {
		case "InspectVolume", "VolumeDevice", "ListVolumes", "DiskUsage", "ContainerList", "RemoveVolume", "CreateVolume":
			t.Errorf("a poll asked Docker: %s", c)
		}
	}
}
