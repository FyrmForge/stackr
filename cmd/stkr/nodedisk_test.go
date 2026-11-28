package main

import (
	"strings"
	"testing"
)

const nd = "/api/v1/admin/nodes/n1/disk"

func TestNodeDiskLsAndAdd(t *testing.T) {
	fake(t, map[string]string{"/disk": `{"has":false}`})
	if code, out, errw := cli(t, "node", "disk", "ls", "n1"); code != 0 || !strings.Contains(out, "has no disk") {
		t.Errorf("ls without a disk = %d %q %q", code, out, errw)
	}
	fake(t, map[string]string{"/disk": `{"has":true,"id":"d1","path":"/mnt/data","kind":"folder","state":"ready","free":53687091200,"total":107374182400,"volumes":3,"stale":true}`})
	if code, out, _ := cli(t, "node", "disk", "ls", "n1"); code != 0 || !strings.Contains(out, "/mnt/data") || !strings.Contains(out, "50 GB free of 100 GB") || !strings.Contains(out, "may be old") {
		t.Errorf("ls = %d %q", code, out)
	}

	fake(t, map[string]string{"/disk": `{"has":true,"id":"d1","path":"/mnt/data","kind":"xfs","state":"ready","free":53687091200,"total":107374182400,"volumes":3,"erroring":true}`})
	if code, out, _ := cli(t, "node", "disk", "ls", "n1"); code != 0 || !strings.Contains(out, "failing") || !strings.Contains(out, "(last good read)") {
		t.Errorf("ls erroring = %d %q", code, out)
	}

	// a refused probe is the server's message and queues nothing
	r := fake(t, map[string]string{"/probe": `{"path":"/mnt/data","code":"not_mount_point","message":"That folder is not a mount point."}`})
	if code, _, errw := cli(t, "node", "disk", "add", "n1", "/mnt/data"); code != 1 || !strings.Contains(errw, "not a mount point") || len(writes(r)) != 1 {
		t.Errorf("add refused = %d %q, wrote %v", code, errw, writes(r))
	}
	r = fake(t, map[string]string{"/probe": `{"path":"/mnt/data","code":"","fs_type":"ext4","free":1073741824,"total":2147483648}`})
	if code, out, errw := cli(t, "node", "disk", "add", "n1", "/mnt/data", "--no-wait"); code != 0 || !strings.Contains(out, "ext4") {
		t.Fatalf("add = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 2 || w[1] != "POST "+nd+` {"path":"/mnt/data"}` {
		t.Errorf("writes = %v", w)
	}
	r = fake(t, nil)
	if code, _, _ := cli(t, "node", "disk", "check", "n1", "--no-wait"); code != 0 || len(writes(r)) != 1 || writes(r)[0] != "POST "+nd+"/check " {
		t.Errorf("check wrote %v", writes(r))
	}
}

const adoptPlan = `{"node":"n1","path":"/mnt/data","disk_id":"d9","old_node":"old","envs":[{"label":"shop / prod","tiles":2}],` +
	`"on_disk":[{"slug":"pg","where":"shop / prod"}],"elsewhere":[],"unclaimed":[],"copies":[],"hosts":[],"refusal":"","candidates":[],"resume_disk":""}`

func TestNodeDiskAdopt(t *testing.T) {
	fake(t, map[string]string{"/adopt-plan": `{"candidates":[{"disk_id":"d9","node":"old","path":"/mnt/old","state":"adopting"}],"resume_disk":""}`})
	if code, out, _ := cli(t, "node", "disk", "adopt", "n1"); code != 0 || !strings.Contains(out, "d9") || !strings.Contains(out, "old") {
		t.Errorf("candidates = %d %q", code, out)
	}

	// no terminal and no -y: it names the question and sends nothing
	r := fake(t, map[string]string{"/adopt-plan": adoptPlan})
	if code, out, errw := cli(t, "node", "disk", "adopt", "n1", "/mnt/data"); code != 3 || !strings.Contains(out, "shop / prod") || len(writes(r)) != 0 {
		t.Errorf("adopt unconfirmed = %d %q %q, wrote %v", code, out, errw, writes(r))
	}
	// --confirm is the typed word and goes through untouched
	r = fake(t, map[string]string{"/adopt-plan": adoptPlan})
	if code, _, errw := cli(t, "node", "disk", "adopt", "n1", "/mnt/data", "--confirm", "n1", "--no-wait"); code != 0 {
		t.Fatalf("adopt = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "POST "+nd+`/adopt {"confirm":"n1","disk_id":"d9","path":"/mnt/data"}` {
		t.Errorf("writes = %v", w)
	}
	// -y sends the expected word
	r = fake(t, map[string]string{"/adopt-plan": adoptPlan})
	if code, _, _ := cli(t, "-y", "node", "disk", "adopt", "n1", "/mnt/data", "--no-wait"); code != 0 || !strings.Contains(writes(r)[0], `"confirm":"n1"`) {
		t.Errorf("adopt -y wrote %v", writes(r))
	}

	// a refusal prints the plan, errors with the server's text, sends nothing
	r = fake(t, map[string]string{"/adopt-plan": strings.Replace(strings.Replace(adoptPlan, `"copies":[]`, `"copies":[{"volume_id":"v1","slug":"pg"}]`, 1), `"refusal":""`, `"refusal":"A copy of pg blocks the adopt."`, 1)})
	code, out, errw := cli(t, "node", "disk", "adopt", "n1", "/mnt/data")
	if code != 1 || !strings.Contains(errw, "A copy of pg blocks the adopt.") || !strings.Contains(out, "--discard pg") || len(writes(r)) != 0 {
		t.Errorf("refused adopt = %d %q %q, wrote %v", code, out, errw, writes(r))
	}
	// -y does not skip the typed slug of a discard
	if code, _, errw = cli(t, "-y", "node", "disk", "adopt", "n1", "/mnt/data", "--discard", "pg"); code != 2 || !strings.Contains(errw, "Pass --confirm pg.") || len(writes(r)) != 0 {
		t.Fatalf("discard -y = %d %q, wrote %v", code, errw, writes(r))
	}
	// --discard removes the copy, with the slug as the word
	if code, _, errw = cli(t, "node", "disk", "adopt", "n1", "/mnt/data", "--discard", "pg", "--confirm", "pg"); code != 0 {
		t.Fatalf("discard = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "POST "+nd+`/adopt-discard {"confirm":"pg","disk_id":"d9","path":"/mnt/data","volume_id":"v1"}` {
		t.Errorf("writes = %v", w)
	}
}

func TestNodeDiskForgetAndMoveAll(t *testing.T) {
	plan := `{"disk_id":"d1","node":"old","gone":false,"volumes":[{"slug":"pg","where":"shop / prod","has_backup":true,"backup":"2026-11-01T08:00:00Z"}],"refusal":""}`
	r := fake(t, map[string]string{"/forget-plan": plan, "/forget": `{"id":"","kind":""}`})
	if code, out, errw := cli(t, "node", "disk", "forget", "d1"); code != 2 || !strings.Contains(out, "backup 2026-11-01") || !strings.Contains(errw, "Pass --confirm old.") || len(writes(r)) != 0 {
		t.Errorf("forget unconfirmed = %d %q %q, wrote %v", code, out, errw, writes(r))
	}
	// -y does not skip a typed word
	if code, _, errw := cli(t, "-y", "node", "disk", "forget", "d1"); code != 2 || !strings.Contains(errw, "Pass --confirm old.") || len(writes(r)) != 0 {
		t.Errorf("forget -y = %d %q, wrote %v", code, errw, writes(r))
	}
	if code, out, errw := cli(t, "node", "disk", "forget", "d1", "--confirm", "old"); code != 0 || !strings.Contains(out, "forgotten") {
		t.Errorf("forget = %d %q %q", code, out, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "POST /api/v1/admin/node-disks/d1/forget "+`{"confirm":"old"}` {
		t.Errorf("writes = %v", w)
	}
	fake(t, map[string]string{"/forget-plan": strings.Replace(plan, `"refusal":""`, `"refusal":"The disk is in use."`, 1)})
	if code, _, errw := cli(t, "node", "disk", "forget", "d1", "-y"); code != 1 || !strings.Contains(errw, "The disk is in use.") {
		t.Errorf("forget refused = %d %q", code, errw)
	}

	mv := `{"groups":[{"volumes":[{"slug":"pg"}],"tiles":["db"],"size_bytes":1073741824}],"skipped":[{"slug":"x","env":"a / b","reason":"on another node"}],"need":1181116006,"free":53687091200,"fits":true,"busy":false}`
	r = fake(t, map[string]string{"/move-all-plan": mv, "/move-all": `{"jobs":[],"refused":""}`})
	if code, out, _ := cli(t, "node", "disk", "move-all", "n1"); code != 3 || !strings.Contains(out, "skip  x") || len(writes(r)) != 0 {
		t.Errorf("move-all unconfirmed = %d %q, wrote %v", code, out, writes(r))
	}
	if code, _, errw := cli(t, "-y", "node", "disk", "move-all", "n1"); code != 0 || len(writes(r)) != 1 || writes(r)[0] != "POST "+nd+"/move-all " {
		t.Errorf("move-all = %d %q, wrote %v", code, errw, writes(r))
	}
	fake(t, map[string]string{"/move-all-plan": strings.Replace(mv, `"busy":false`, `"busy":true`, 1)})
	if code, _, errw := cli(t, "-y", "node", "disk", "move-all", "n1"); code != 1 || !strings.Contains(errw, "already moving") {
		t.Errorf("busy = %d %q", code, errw)
	}
	fake(t, map[string]string{"/move-all-plan": strings.Replace(mv, `"fits":true`, `"fits":false`, 1)})
	if code, _, errw := cli(t, "-y", "node", "disk", "move-all", "n1"); code != 1 || !strings.Contains(errw, "need") {
		t.Errorf("does not fit = %d %q", code, errw)
	}
}

func TestVolumeMoveToDisk(t *testing.T) {
	const org = "/api/v1/orgs/acme/volumes/v1"
	answers := map[string]string{
		"/volumes":       `[{"id":"v1","slug":"data","scope_kind":"org"}]`,
		"/move-plan":     `{"slug":"data","env":"shop / prod","tiles":["api"],"size_bytes":1073741824,"need":1181116006,"free":53687091200,"fits":true,"finish":false}`,
		"/move-to-disk":  `{}`,
		"/discard-plain": ``,
		"/discard-bound": ``,
	}
	r := fake(t, answers)
	if code, out, _ := cli(t, "volume", "move-to-disk", "data"); code != 3 || !strings.Contains(out, "tiles stop while it copies: api") || len(writes(r)) != 0 {
		t.Errorf("unconfirmed = %d %q, wrote %v", code, out, writes(r))
	}
	if code, _, errw := cli(t, "-y", "volume", "move-to-disk", "data", "--no-wait"); code != 0 || len(writes(r)) != 1 || writes(r)[0] != "POST "+org+"/move-to-disk " {
		t.Errorf("move = %d %q, wrote %v", code, errw, writes(r))
	}
	answers["/move-plan"] = strings.Replace(answers["/move-plan"], `"fits":true`, `"fits":false`, 1)
	r = fake(t, answers)
	if code, _, errw := cli(t, "-y", "volume", "move-to-disk", "data"); code != 1 || !strings.Contains(errw, "the disk has") || len(writes(r)) != 0 {
		t.Errorf("does not fit = %d %q, wrote %v", code, errw, writes(r))
	}

	r = fake(t, answers)
	if code, _, errw := cli(t, "volume", "discard", "data", "--confirm", "data"); code != 0 {
		t.Fatalf("discard = %d %q", code, errw)
	}
	if code, _, errw := cli(t, "volume", "discard", "data", "--bound", "-y"); code != 2 || !strings.Contains(errw, "Pass --confirm data.") || len(writes(r)) != 1 {
		t.Fatalf("discard --bound -y = %d %q, wrote %v", code, errw, writes(r))
	}
	if code, _, _ := cli(t, "volume", "discard", "data", "--bound", "--confirm", "data"); code != 0 {
		t.Fatal("discard --bound failed")
	}
	if w := writes(r); len(w) != 2 || w[0] != "POST "+org+`/discard-plain {"confirm":"data"}` || w[1] != "POST "+org+`/discard-bound {"confirm":"data"}` {
		t.Errorf("writes = %v", w)
	}
}

const devs = `{"devices":[` +
	`{"path":"/dev/sdb","id":"ata-X_1","bytes":536870912000,"blocked":"","signature":""},` +
	`{"path":"/dev/sdc","id":"ata-X_2","model":"WDC","serial":"WD-77","bytes":2147483648000,"signature":"filesystem","fs_type":"ext4"},` +
	`{"path":"/dev/sda","id":"ata-X_0","bytes":85899345920,"blocked":"system"}]}`

func TestNodeDiskDevicesAndAddDevice(t *testing.T) {
	r := fake(t, map[string]string{"/devices": devs})
	if code, out, errw := cli(t, "node", "disk", "devices", "n1"); code != 0 || !strings.Contains(out, "sdb") || !strings.Contains(out, "not available: system") || !strings.Contains(out, "holds data: filesystem") || len(writes(r)) != 1 {
		t.Errorf("devices = %d %q %q", code, out, errw)
	}

	// no --confirm and no terminal: nothing is formatted, and -y does not answer it
	r = fake(t, map[string]string{"/devices": devs, "/add-device": `{"id":"j1","kind":"disk-add-device","state":"queued"}`})
	for _, args := range [][]string{{"node", "disk", "add", "n1", "--device", "sdb"}, {"-y", "node", "disk", "add", "n1", "--device", "sdb"}} {
		if code, _, errw := cli(t, args...); code != 1 || !strings.Contains(errw, "--confirm") {
			t.Errorf("%v = %d %q, want the typed word asked for", args, code, errw)
		}
	}
	for _, q := range writes(r) {
		if strings.Contains(q, "add-device") {
			t.Fatalf("formatted without the typed word: %v", writes(r))
		}
	}
	// the typed word sends the whole listed disk; force stays off
	r = fake(t, map[string]string{"/devices": devs, "/add-device": `{"id":"j1","kind":"disk-add-device","state":"queued"}`})
	if code, _, errw := cli(t, "node", "disk", "add", "n1", "--device", "ata-X_1", "--confirm", "sdb", "--no-wait"); code != 0 {
		t.Fatalf("add = %d %q", code, errw)
	}
	w := writes(r)
	if len(w) != 2 || !strings.HasPrefix(w[1], "POST "+nd+"/add-device ") || !strings.Contains(w[1], `"confirm":"sdb"`) || !strings.Contains(w[1], `"force":false`) || !strings.Contains(w[1], `"path":"/dev/sdb"`) {
		t.Errorf("writes = %v", w)
	}
	// a disk that holds data needs --format-anyway; a blocked one is refused
	r = fake(t, map[string]string{"/devices": devs})
	if code, _, errw := cli(t, "node", "disk", "add", "n1", "--device", "sdc", "--confirm", "sdc"); code != 1 || !strings.Contains(errw, "--format-anyway") {
		t.Errorf("holds data = %d %q", code, errw)
	}
	if code, _, errw := cli(t, "node", "disk", "add", "n1", "--device", "sda", "--confirm", "sda", "--format-anyway"); code != 1 || !strings.Contains(errw, "cannot be used") {
		t.Errorf("blocked = %d %q", code, errw)
	}
	for _, q := range writes(r) {
		if strings.Contains(q, "add-device") {
			t.Fatalf("a refused disk was sent: %v", writes(r))
		}
	}
	// Format anyway on a bare kernel name: the name is not a confirmation (the
	// list and the format would compare the disk with itself), the serial or the
	// by-id name is; the disk's identity is printed before it is asked
	r = fake(t, map[string]string{"/devices": devs, "/add-device": `{"id":"j1","kind":"disk-add-device","state":"queued"}`})
	if code, _, errw := cli(t, "node", "disk", "add", "n1", "--device", "sdc", "--confirm", "sdc", "--format-anyway", "--no-wait"); code != 1 || !strings.Contains(errw, "WD-77") || !strings.Contains(errw, "ata-X_2") {
		t.Errorf("format anyway with the kernel name = %d %q", code, errw)
	}
	for _, q := range writes(r) {
		if strings.Contains(q, "add-device") {
			t.Fatalf("formatted on the kernel name: %v", writes(r))
		}
	}
	for _, typed := range []string{"WD-77", "ata-X_2"} {
		r = fake(t, map[string]string{"/devices": devs, "/add-device": `{"id":"j1","kind":"disk-add-device","state":"queued"}`})
		code, out, errw := cli(t, "node", "disk", "add", "n1", "--device", "sdc", "--confirm", typed, "--format-anyway", "--no-wait")
		if w := writes(r); code != 0 || len(w) != 2 || !strings.Contains(w[1], `"force":true`) || !strings.Contains(w[1], `"confirm":"sdc"`) {
			t.Errorf("format anyway typing %q = %d %q, wrote %v", typed, code, errw, w)
		}
		if !strings.Contains(out+errw, "model WDC, serial WD-77, id ata-X_2") {
			t.Errorf("the disk was not described: %q", errw)
		}
	}
	// a disk with no serial and no by-id link has nothing stable to confirm with: refused
	bare := `{"devices":[{"path":"/dev/vdb","id":"/dev/vdb","bytes":1073741824,"signature":"filesystem","fs_type":"ext4"}]}`
	r = fake(t, map[string]string{"/devices": bare, "/add-device": `{"id":"j1","kind":"disk-add-device","state":"queued"}`})
	if code, _, errw := cli(t, "node", "disk", "add", "n1", "--device", "vdb", "--confirm", "vdb", "--format-anyway", "--no-wait"); code != 1 || !strings.Contains(errw, "no serial or stable id") {
		t.Errorf("format anyway with nothing stable = %d %q", code, errw)
	}
	for _, q := range writes(r) {
		if strings.Contains(q, "add-device") {
			t.Fatalf("formatted with the kernel name as its own proof: %v", writes(r))
		}
	}
	// named by its by-id name, the kernel name is a fine confirmation
	r = fake(t, map[string]string{"/devices": devs, "/add-device": `{"id":"j1","kind":"disk-add-device","state":"queued"}`})
	if code, _, errw := cli(t, "node", "disk", "add", "n1", "--device", "ata-X_2", "--confirm", "sdc", "--format-anyway", "--no-wait"); code != 0 || !strings.Contains(writes(r)[1], `"force":true`) {
		t.Errorf("format anyway by id = %d %q, wrote %v", code, errw, writes(r))
	}
}

func TestNodeDiskGrowAndVolumeCap(t *testing.T) {
	r := fake(t, nil)
	if code, _, _ := cli(t, "node", "disk", "grow", "n1", "--no-wait"); code != 0 || len(writes(r)) != 1 || writes(r)[0] != "POST "+nd+"/grow " {
		t.Errorf("grow wrote %v", writes(r))
	}
	for in, want := range map[string]string{"20GB": "20480", "1500MB": "1500", "2tb": "2097152", "none": "null"} {
		r = fake(t, map[string]string{"/cap": `{"max_size_mb":null,"enforced":true}`, "/volumes": `[{"id":"v1","slug":"pg","scope_kind":"env"}]`})
		if code, _, errw := cli(t, "volume", "cap", "v1", in); code != 0 || !strings.HasSuffix(writes(r)[0], `{"max_size_mb":`+want+`}`) {
			t.Errorf("cap %s = %d %q, wrote %v", in, code, errw, writes(r))
		}
	}
	for _, bad := range []string{"0", "abc", "-3GB", "2000000TB"} {
		if code, _, _ := cli(t, "volume", "cap", "v1", bad); code != 2 {
			t.Errorf("cap %q = %d, want 2", bad, code)
		}
	}
}

func TestNodeDiskMovePanel(t *testing.T) {
	state := `{"applicable":true,"reason":"","on_disk":false}`
	plan := `{"size_bytes":52428800,"disk_free":53687091200,"system_free":10737418240,"restarts":"The panel restarts.","warning":"After this, the whole control plane depends on this one disk.","refusal":""}`
	r := fake(t, map[string]string{"/panel-db": state, "/move-panel-plan": plan, "/move-panel": `{"id":"","kind":""}`})
	if code, out, _ := cli(t, "node", "disk", "move-panel", "n1"); code != 3 || !strings.Contains(out, "depends on this one disk") || len(writes(r)) != 0 {
		t.Errorf("unconfirmed = %d %q, wrote %v", code, out, writes(r))
	}
	if code, _, errw := cli(t, "-y", "node", "disk", "move-panel", "n1", "--no-wait"); code != 0 || len(writes(r)) != 1 || writes(r)[0] != "POST "+nd+`/move-panel {"back":false}` {
		t.Errorf("move = %d %q, wrote %v", code, errw, writes(r))
	}
	// back: only from the disk
	if code, _, errw := cli(t, "-y", "node", "disk", "move-panel", "n1", "--back"); code != 1 || !strings.Contains(errw, "system disk") {
		t.Errorf("back from the system disk = %d %q", code, errw)
	}
	r = fake(t, map[string]string{"/panel-db": `{"applicable":true,"on_disk":true}`, "/move-panel-plan": plan, "/move-panel": `{"id":"","kind":""}`})
	if code, _, errw := cli(t, "-y", "node", "disk", "move-panel", "n1", "--back", "--no-wait"); code != 0 || writes(r)[0] != "POST "+nd+`/move-panel {"back":true}` {
		t.Errorf("back = %d %q, wrote %v", code, errw, writes(r))
	}
	fake(t, map[string]string{"/panel-db": state, "/move-panel-plan": strings.Replace(plan, `"refusal":""`, `"refusal":"An update is running."`, 1)})
	if code, _, errw := cli(t, "-y", "node", "disk", "move-panel", "n1"); code != 1 || !strings.Contains(errw, "An update is running.") {
		t.Errorf("refused = %d %q", code, errw)
	}
	fake(t, map[string]string{"/panel-db": `{"applicable":false,"reason":"Only the controller holds the panel database."}`})
	if code, _, errw := cli(t, "-y", "node", "disk", "move-panel", "n1"); code != 1 || !strings.Contains(errw, "controller") {
		t.Errorf("worker = %d %q", code, errw)
	}
}

func TestNodeDiskForgetWarnsWhenObserveIsOnTheDisk(t *testing.T) {
	plan := `{"disk_id":"d1","node":"old","gone":false,"volumes":[],"refusal":"","observe_on":true}`
	fake(t, map[string]string{"/forget-plan": plan})
	if _, out, _ := cli(t, "node", "disk", "forget", "d1"); !strings.Contains(out, "Logs and metrics are on this disk and are lost.") {
		t.Errorf("no warning: %q", out)
	}
}
