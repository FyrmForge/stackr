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
	if code, out, _ := cli(t, "node", "disk", "forget", "d1"); code != 3 || !strings.Contains(out, "backup 2026-11-01") || len(writes(r)) != 0 {
		t.Errorf("forget unconfirmed = %d %q, wrote %v", code, out, writes(r))
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
	if code, _, _ := cli(t, "volume", "discard", "data", "--bound", "-y"); code != 0 {
		t.Fatal("discard --bound failed")
	}
	if w := writes(r); len(w) != 2 || w[0] != "POST "+org+`/discard-plain {"confirm":"data"}` || w[1] != "POST "+org+`/discard-bound {"confirm":"data"}` {
		t.Errorf("writes = %v", w)
	}
}
