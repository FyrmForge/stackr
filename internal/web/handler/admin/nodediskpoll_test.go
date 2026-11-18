package admin_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// folderRow gives the disk a folder row (and the folder) for name.
func folderRow(t *testing.T, s *webtest.Site, id, name string) {
	t.Helper()
	d, err := s.Store.NodeDisks.GetByNode(t.Context(), id)
	must(t, err)
	mk := reflect.ValueOf(s.Store.DiskFolders.Create)
	f := reflect.New(mk.Type().In(1)).Elem()
	f.FieldByName("ID").SetString("f-" + name)
	f.FieldByName("DiskID").SetString(d.ID)
	f.FieldByName("Name").SetString(name)
	if out := mk.Call([]reflect.Value{reflect.ValueOf(t.Context()), f}); !out[0].IsNil() {
		t.Fatal(out[0].Interface())
	}
	must(t, os.MkdirAll(filepath.Join(s.Disk.Dir("/data"), d.ID, name), 0o755))
}

// A poll is cheap: the node drawer and both kinds of volume drawer never ask
// Docker about a volume, and a plain Docker volume shows no size.
func TestDrawerPollsNeverAskDockerAboutVolumes(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	addedDisk(t, s, root, id, "/data")
	onDisk := s.Volume(t, "env", s.Tile.Env, "pgdata")
	plain := s.Volume(t, "env", s.Tile.Env, "plain")
	folderRow(t, s, id, "pgdata")

	n := len(s.Docker.Calls())
	node := s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String()
	has(t, "node", node, "pgdata")
	disk := s.As(t, root, "GET", "/acme/shop/dev/-/volumes/"+onDisk, nil).Body.String()
	has(t, "disk volume", disk, "Node disk")
	legacy := s.As(t, root, "GET", "/acme/shop/dev/-/volumes/"+plain, nil).Body.String()
	has(t, "plain volume", legacy, "Docker storage")
	if strings.Contains(legacy, ">Size</p>") {
		t.Errorf("a plain volume shows a size\n%s", legacy)
	}
	for _, c := range s.Docker.Calls()[n:] {
		switch c.Method {
		case "InspectVolume", "VolumeDevice", "ListVolumes", "DiskUsage", "ContainerList":
			t.Errorf("a poll asked Docker: %s", c)
		}
	}
}

// Only the overview builds the disk section.
func TestNodeDrawerOtherTabsSkipTheDiskSection(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	addedDisk(t, s, root, id, "/data")
	body := s.As(t, root, "GET", "/-/admin/nodes/"+id+"?tab=tags", nil).Body.String()
	if strings.Contains(body, "Node disk") {
		t.Error("the tags tab carries the disk section")
	}
}

// The Add disk dialog's typed path must not stall the poll: the live region
// has no dirty guard.
func TestNodeLivePollHasNoDirtyGuard(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	body := s.As(t, root, "GET", "/-/admin/nodes/"+diskNode(t, s), nil).Body.String()
	has(t, "poller", body, `data-poll-guard="region rows fields"`)
}

// ioHost fails every call with an I/O error.
type ioHost struct{}

func (ioHost) Disk(context.Context, string, hostdisk.Request) (hostdisk.Result, error) {
	return hostdisk.Result{Error: &hostdisk.Error{Code: hostdisk.CodeIO, Message: "read /data: input/output error"}}, nil
}

func TestProbeIOErrorShowsItsMessageAndClearsTheAnswer(t *testing.T) {
	s := webtest.NewWith(t, []service.Option{service.WithDiskHost(ioHost{})})
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	rec := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/probe",
		url.Values{"path": {"/data"}, "checked": {"/data"}, "net": {"1"}, "net_ok": {"1"}})
	body := rec.Body.String()
	has(t, "io", body, "input/output error", `id="disk-checked" name="checked" value=""`)
	if strings.Contains(body, "did not answer") {
		t.Errorf("an I/O failure reads as a dead node\n%s", body)
	}
}

func TestProbeSymlinkRefusalNamesTheRealPath(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	s.Disk.Mount(t, "/real")
	must(t, os.Symlink("/real", s.Disk.Dir("/alias")))
	body := s.As(t, root, "POST", "/-/admin/nodes/"+id+"/disk/probe", url.Values{"path": {"/alias"}}).Body.String()
	has(t, "alias", body, "Use the real path: /real.")
}

// While an add is queued and no row exists yet, the section says so and
// offers no second Add disk. The job is seeded waiting, so nothing runs it.
func TestNodeDiskQueuedAddShowsAdding(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	id := diskNode(t, s)
	mk := reflect.ValueOf(s.Store.Jobs.Create)
	j := reflect.New(mk.Type().In(1)).Elem()
	j.FieldByName("ID").SetString("j-add")
	j.FieldByName("Kind").SetString("disk-add")
	j.FieldByName("State").SetString("waiting")
	j.FieldByName("LockSet").Set(reflect.MakeSlice(j.FieldByName("LockSet").Type(), 0, 0))
	j.FieldByName("Payload").SetString(`{"node_id":"` + id + `","path":"/data"}`)
	j.FieldByName("CreatedAt").Set(reflect.ValueOf(time.Now()))
	if out := mk.Call([]reflect.Value{reflect.ValueOf(t.Context()), j}); !out[0].IsNil() {
		t.Fatal(out[0].Interface())
	}
	card := section(t, s.As(t, root, "GET", "/-/admin/nodes/"+id, nil).Body.String())
	has(t, "queued", card, "Adding the disk.")
	if strings.Contains(card, `id="disk-use"`) {
		t.Errorf("Add disk is offered while an add is queued\n%s", card)
	}
}

// errHost fails every call with err, as a transport or a plain failure.
type errHost struct{ err error }

func (h errHost) Disk(context.Context, string, hostdisk.Request) (hostdisk.Result, error) {
	return hostdisk.Result{}, h.err
}

func TestProbeSilentNodeSaysDidNotAnswerOtherErrorsSurface(t *testing.T) {
	post := func(err error) *httptest.ResponseRecorder {
		s := webtest.NewWith(t, []service.Option{service.WithDiskHost(errHost{err})})
		root := s.Session(t, s.User(t, "root@x.test", true))
		return s.As(t, root, "POST", "/-/admin/nodes/"+diskNode(t, s)+"/disk/probe", url.Values{"path": {"/data"}})
	}
	rec := post(fmt.Errorf("probe: %w", context.DeadlineExceeded))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "did not answer. Try again.") {
		t.Errorf("transport failure: %d %s", rec.Code, rec.Body.String())
	}
	rec = post(errors.New("database is locked"))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "did not answer") {
		t.Errorf("plain error: %d %s", rec.Code, rec.Body.String())
	}
}
