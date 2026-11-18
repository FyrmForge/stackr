package api_test

import (
	"strings"
	"testing"
)

// The node disk verbs are server-admin: nobody else reaches them, and the
// service's refusal comes back with its message.
func TestNodeDiskAPI(t *testing.T) {
	w := newWorld(t)
	for _, p := range []struct{ method, path, body string }{
		{"GET", "/admin/nodes/x/disk", ""},
		{"POST", "/admin/nodes/x/disk", `{"path":"/mnt/data"}`},
		{"POST", "/admin/nodes/x/disk/probe", `{"path":"/mnt/data"}`},
		{"POST", "/admin/nodes/x/disk/check", ""},
		{"POST", "/admin/nodes/x/disk/devices", ""},
		{"POST", "/admin/nodes/x/disk/add-device", `{"device":{"path":"/dev/sdb","id":"sdb"},"confirm":"sdb"}`},
		{"POST", "/admin/nodes/x/disk/grow", ""},
		{"GET", "/admin/nodes/x/disk/adopt-plan?path=/mnt/data", ""},
		{"POST", "/admin/nodes/x/disk/adopt", `{"disk_id":"d","path":"/mnt/data","confirm":"x"}`},
		{"POST", "/admin/nodes/x/disk/adopt-discard", `{"disk_id":"d","path":"/mnt/data","volume_id":"v","confirm":"v"}`},
		{"GET", "/admin/nodes/x/disk/move-all-plan", ""},
		{"POST", "/admin/nodes/x/disk/move-all", ""},
		{"GET", "/admin/node-disks/d/forget-plan", ""},
		{"POST", "/admin/node-disks/d/forget", `{"confirm":"d"}`},
	} {
		if code, _ := w.do(t, "", p.method, p.path, p.body); code != 401 {
			t.Errorf("%s %s anonymous = %d, want 401", p.method, p.path, code)
		}
		if code, _ := w.do(t, w.owner, p.method, p.path, p.body); code != 403 {
			t.Errorf("%s %s owner = %d, want 403", p.method, p.path, code)
		}
	}

	// an admin on the controller: no disk yet, so the disk is empty and the
	// check has nothing to check
	if code, body := w.do(t, w.admin, "GET", "/admin/nodes/nope/disk", ""); code != 404 {
		t.Errorf("disk of an unknown node = %d %s, want 404", code, body)
	}
	_, body := w.do(t, w.admin, "GET", "/nodes", "")
	i := strings.Index(body, `"name":"`)
	if i < 0 {
		t.Fatalf("nodes = %s", body)
	}
	node := body[i+8:]
	node, _, _ = strings.Cut(node, `"`)
	if code, body := w.do(t, w.admin, "GET", "/admin/nodes/"+node+"/disk", ""); code != 200 || !strings.Contains(body, `"has":false`) {
		t.Errorf("disk = %d %s, want has false", code, body)
	}
	// a refusal surfaces with its message
	if code, body := w.do(t, w.admin, "POST", "/admin/nodes/"+node+"/disk/probe", `{"path":"mnt/data"}`); code != 400 || !strings.Contains(body, "full path") {
		t.Errorf("probe of a relative path = %d %s, want 400 with the message", code, body)
	}
	if code, body := w.do(t, w.admin, "POST", "/admin/node-disks/nope/forget", `{"confirm":"x"}`); code != 404 {
		t.Errorf("forget of an unknown disk = %d %s, want 404", code, body)
	}
}

// Moving a volume to the disk is tile.write in the volume's org; another
// org's key and a reader are refused.
func TestVolumeMoveAPI(t *testing.T) {
	w := newWorld(t)
	vol := w.env.Volume(t, "stack", w.tile.Stack, "data")
	reader := w.env.User(t, "reader@x", false)
	w.env.Member(t, w.acme, reader, "member")
	rk := w.env.APIKey(t, reader, w.acme)
	base := "/orgs/acme/volumes/" + vol
	for _, p := range []struct{ method, path, body string }{
		{"GET", base + "/move-plan", ""},
		{"POST", base + "/move-to-disk", ""},
		{"POST", base + "/discard-plain", `{"confirm":"data"}`},
		{"POST", base + "/discard-bound", `{"confirm":"data"}`},
	} {
		if code, _ := w.do(t, "", p.method, p.path, p.body); code != 401 {
			t.Errorf("%s %s anonymous = %d, want 401", p.method, p.path, code)
		}
		if code, _ := w.do(t, w.stranger, p.method, p.path, p.body); code != 404 && code != 403 {
			t.Errorf("%s %s other org = %d, want 404", p.method, p.path, code)
		}
		if code, _ := w.do(t, rk, p.method, p.path, p.body); code != 403 {
			t.Errorf("%s %s reader = %d, want 403", p.method, p.path, code)
		}
	}
	// an owner passes the permission and meets the service's refusal
	if code, body := w.do(t, w.owner, "POST", base+"/move-to-disk", ""); code != 403 || !strings.Contains(body, "disk is not ready") {
		t.Errorf("owner move-to-disk = %d %s, want the refusal with its message", code, body)
	}
	if code, _ := w.do(t, "", "PUT", base+"/cap", `{"max_size_mb":100}`); code != 401 {
		t.Errorf("cap anonymous = %d, want 401", code)
	}
	if code, _ := w.do(t, w.stranger, "PUT", base+"/cap", `{"max_size_mb":100}`); code != 404 && code != 403 {
		t.Errorf("cap other org = %d, want 404", code)
	}
	// the cap: saved and reported not enforced on a volume with no xfs disk, removed with null,
	// and a size over 1 PB is its own refusal
	if code, body := w.do(t, w.owner, "PUT", base+"/cap", `{"max_size_mb":100}`); code != 200 || !strings.Contains(body, `"max_size_mb":100`) || !strings.Contains(body, `"enforced":false`) {
		t.Errorf("owner cap = %d %s", code, body)
	}
	if code, body := w.do(t, w.owner, "PUT", base+"/cap", `{"max_size_mb":null}`); code != 200 || !strings.Contains(body, `"max_size_mb":null`) {
		t.Errorf("owner cap none = %d %s", code, body)
	}
	if code, body := w.do(t, w.owner, "PUT", base+"/cap", `{"max_size_mb":2000000000}`); code != 400 || !strings.Contains(body, "Give a size up to 1 PB.") {
		t.Errorf("owner cap over 1 PB = %d %s", code, body)
	}
}
