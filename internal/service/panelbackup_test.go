package service

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/s3"
)

// fakeS3 is a path-style bucket in memory: PUT, GET, DELETE and list-type=2.
type fakeS3 struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	f := &fakeS3{objs: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) keys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rest := strings.TrimPrefix(r.URL.Path, "/")
	_, key, _ := strings.Cut(rest, "/") // bucket/key
	switch {
	case r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objs[key] = b
	case r.Method == http.MethodDelete:
		delete(f.objs, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && key == "":
		prefix := r.URL.Query().Get("prefix")
		type obj struct{ Key string }
		res := struct {
			XMLName xml.Name `xml:"ListBucketResult"`
			Objs    []obj    `xml:"Contents"`
		}{}
		for k := range f.objs {
			if strings.HasPrefix(k, prefix) {
				res.Objs = append(res.Objs, obj{k})
			}
		}
		_ = xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodGet:
		if b, ok := f.objs[key]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}
}

func (w *world) globalS3(t *testing.T, endpoint string) BackupDest {
	t.Helper()
	d, err := w.orch.CreateBackupDest(context.Background(), nil, BackupDestSpec{
		Name: "offsite", Endpoint: endpoint, Bucket: "b", AccessKey: "a", SecretKey: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A scheduled panel backup goes to panel_backup_dest, is marked schedule and
// prunes that destination to panel_backup_keep.
func TestScheduledPanelBackupToDest(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s3, srv := newFakeS3(t)
	d := w.globalS3(t, srv.URL)
	must(t, w.orch.SetSetting(ctx, "panel_backup_dest", d.ID))
	must(t, w.orch.SetSetting(ctx, "panel_backup_keep", "2"))
	prefix := "stackr/_panel/" + w.orch.cfg.InstallID + "/scheduled/"
	for _, k := range []string{"20200101-000000-panel.tar.gz.age", "20200102-000000-panel.tar.gz.age", "20200103-000000-panel.tar.gz.age"} {
		s3.objs[prefix+k] = []byte("old")
	}
	var log bytes.Buffer
	r, err := w.orch.panelBackup(ctx, &log, "schedule")
	if err != nil {
		t.Fatal(err)
	}
	if r.Trigger != "schedule" || r.DestID != d.ID {
		t.Errorf("run = %q on %q", r.Trigger, r.DestID)
	}
	got := s3.keys(prefix)
	if len(got) != 2 || got[1] != r.ObjectKey {
		t.Errorf("after prune: %v (new %s)", got, r.ObjectKey)
	}
}

// A destination that is gone falls back to local, and the log says so.
func TestScheduledPanelBackupDestGone(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, srv := newFakeS3(t)
	d := w.globalS3(t, srv.URL)
	must(t, w.orch.SetSetting(ctx, "panel_backup_dest", d.ID))
	must(t, w.orch.backups.Delete(ctx, d, false)) // as if the row went another way
	var log bytes.Buffer
	r, err := w.orch.panelBackup(ctx, &log, "schedule")
	if err != nil {
		t.Fatal(err)
	}
	local, _ := w.orch.localDest(ctx)
	if r.DestID != local.ID || r.Trigger != "schedule" {
		t.Errorf("run = %q on %q, want local %q", r.Trigger, r.DestID, local.ID)
	}
	if !strings.Contains(log.String(), "warning: panel backup destination") {
		t.Errorf("log = %q", log.String())
	}
}

// The manual button always goes local and is marked manual, whatever the
// scheduled destination is.
func TestManualPanelBackupStaysLocal(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, srv := newFakeS3(t)
	d := w.globalS3(t, srv.URL)
	must(t, w.orch.SetSetting(ctx, "panel_backup_dest", d.ID))
	r, err := w.orch.panelBackup(ctx, io.Discard, "manual")
	if err != nil {
		t.Fatal(err)
	}
	local, _ := w.orch.localDest(ctx)
	if r.DestID != local.ID || r.Trigger != "manual" {
		t.Errorf("run = %q on %q", r.Trigger, r.DestID)
	}
}

// The archive takes the proxy volume's tar through the docker tool.
func TestPanelBackupReadsProxyVolume(t *testing.T) {
	w := newWorld(t)
	if _, err := w.orch.panelBackup(context.Background(), io.Discard, "manual"); err != nil {
		t.Fatal(err)
	}
	for _, c := range w.fake.Calls() {
		if c.Method == "TarVolume" && len(c.Args) > 0 && c.Args[0] == "stackr-caddy" {
			return
		}
	}
	t.Errorf("no TarVolume of stackr-caddy: %v", w.fake.Calls())
}

// Scheduled archives prune to panel_backup_keep; the pre-upgrade and manual
// ones sit under their own prefixes with a fixed count, so a scheduled run
// never deletes the archive taken before an upgrade.
func TestPanelBackupPrunesByTrigger(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.orch.cfg.InstallID = "box1"
	must(t, w.orch.SetSetting(ctx, "panel_backup_keep", "1"))
	local, err := w.orch.localDest(ctx)
	must(t, err)
	list := func(prefix string) []string {
		ks, err := (s3.Local{Dir: local.Endpoint}).List(ctx, prefix)
		must(t, err)
		return ks
	}
	up, err := w.orch.panelBackup(ctx, io.Discard, "upgrade")
	must(t, err)
	// Fifteen manual archives already there: one more keeps the newest 14.
	manual := "stackr/_panel/" + w.orch.cfg.InstallID + "/manual/"
	for i := range 15 {
		k := manual + "2020010" + string(rune('a'+i)) + "-000000-panel.tar.gz.age"
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(local.Endpoint, k)), 0o700))
		must(t, os.WriteFile(filepath.Join(local.Endpoint, k), []byte("old"), 0o600))
	}
	if _, err := w.orch.panelBackup(ctx, io.Discard, "manual"); err != nil {
		t.Fatal(err)
	}
	if n := len(list(manual)); n != 14 {
		t.Errorf("manual archives kept = %d, want 14", n)
	}
	for range 2 {
		if _, err := w.orch.panelBackup(ctx, io.Discard, "schedule"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(list("stackr/_panel/" + w.orch.cfg.InstallID + "/scheduled/")); n != 1 {
		t.Errorf("scheduled archives kept = %d, want 1", n)
	}
	if ks := list(up.ObjectKey); len(ks) != 1 {
		t.Errorf("the pre-upgrade archive %s was pruned", up.ObjectKey)
	}
}

// A manual and a scheduled panel backup never supersede each other, and both
// hold the panel lock so neither overlaps an upgrade.
func TestPanelBackupLocks(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	manual, err := w.orch.PanelBackupNow(ctx)
	must(t, err)
	var payload struct{ Scheduled bool }
	payload.Scheduled = true
	b, _ := json.Marshal(payload)
	sched, err := w.orch.jobs.Enqueue(ctx, kindPanelBackup, w.orch.panelLock(true), string(b), nil)
	must(t, err)
	for _, j := range []Job{manual, sched} {
		if !slicesContains(j.LockSet, "panel") {
			t.Errorf("lock set %v lacks the panel lock", j.LockSet)
		}
	}
	if job.Supersedes(sched, manual) || job.Supersedes(manual, sched) {
		t.Errorf("a manual run and a scheduled one supersede each other: %v vs %v", manual.LockSet, sched.LockSet)
	}
	if !job.Overlaps(manual.LockSet, sched.LockSet) {
		t.Error("the two share no lock: they would run together")
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// The panel's own backup destination cannot be deleted from under it.
func TestDeletePanelDestRefused(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, srv := newFakeS3(t)
	d := w.globalS3(t, srv.URL)
	must(t, w.orch.SetSetting(ctx, "panel_backup_dest", d.ID))
	if err := w.orch.DeleteBackupDest(ctx, "", d.ID); err == nil {
		t.Error("deleted the destination the panel backup names")
	}
	must(t, w.orch.SetSetting(ctx, "panel_backup_dest", ""))
	if err := w.orch.DeleteBackupDest(ctx, "", d.ID); err != nil {
		t.Errorf("delete once unused = %v", err)
	}
}

// A crash leaves unencrypted tars in the spool dir; boot clears it.
func TestBootClearsScratch(t *testing.T) {
	dir := t.TempDir()
	scratch := filepath.Join(dir, "backups", "scratch")
	must(t, os.MkdirAll(scratch, 0o700))
	left := filepath.Join(scratch, "caddy-1.tar.gz")
	must(t, os.WriteFile(left, []byte("certs"), 0o600))
	orch, err := New(
		Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"},
		WithDocker(dockerfake.New()),
		WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }),
	)
	must(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	if _, err := os.Stat(left); err == nil {
		t.Error("boot left the spool file")
	}
}

// The Backups form checks every value before it saves any, and pushes the
// proxy config once.
func TestSetSettingsAllOrNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	before := func() int { w.mu.Lock(); defer w.mu.Unlock(); return len(w.pushed) }
	n := before()
	err := w.orch.SetSettings(ctx, map[string]string{
		"panel_backup_schedule": "0 5 * * *",
		"panel_backup_keep":     "7",
		"panel_backup_enabled":  "banana",
	})
	if err == nil {
		t.Fatal("a bad value was taken")
	}
	if v, _ := w.orch.Setting(ctx, "panel_backup_schedule"); v != "0 3 * * *" {
		t.Errorf("schedule saved despite the failed save: %q", v)
	}
	if v, _ := w.orch.Setting(ctx, "panel_backup_keep"); v != "14" {
		t.Errorf("keep saved despite the failed save: %q", v)
	}
	must(t, w.orch.SetSettings(ctx, map[string]string{
		"panel_backup_schedule": "0 5 * * *",
		"panel_backup_keep":     "7",
		"panel_backup_enabled":  "false",
	}))
	if got := before() - n; got != 1 {
		t.Errorf("proxy pushed %d times for one save, want 1", got)
	}
	if slicesContains(w.orch.sched.Names(), "panel-backup") {
		t.Errorf("panel backup still scheduled after switching it off: %v", w.orch.sched.Names())
	}
}
