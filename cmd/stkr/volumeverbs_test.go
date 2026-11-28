package main

import (
	"strings"
	"testing"
)

func TestVolumeRmWipeRestore(t *testing.T) {
	const org = "/api/v1/orgs/acme/volumes/v1"
	r := fake(t, map[string]string{
		"/volumes": `[{"id":"v1","slug":"data","scope_kind":"org"}]`,
		"/delete":  `{"id":"j1","kind":"volume-delete","state":"queued"}`,
		"/wipe":    `{"id":"j2","kind":"volume-wipe","state":"queued"}`,
	})
	if code, _, _ := cli(t, "volume", "rm", "data"); code != 3 || len(writes(r)) != 0 {
		t.Errorf("rm unconfirmed = %d, wrote %v", code, writes(r))
	}
	if code, _, errw := cli(t, "-y", "volume", "rm", "data"); code != 0 || writes(r)[0] != "DELETE "+org+" " {
		t.Errorf("rm = %d %q, wrote %v", code, errw, writes(r))
	}
	if code, _, errw := cli(t, "-y", "volume", "rm", "data", "--backup-first", "--no-wait"); code != 0 || writes(r)[1] != "POST "+org+"/delete " {
		t.Errorf("rm --backup-first = %d %q, wrote %v", code, errw, writes(r))
	}
	if code, _, _ := cli(t, "volume", "wipe", "data", "--no-wait"); code != 3 || len(writes(r)) != 2 {
		t.Errorf("wipe unconfirmed = %d, wrote %v", code, writes(r))
	}
	if code, _, errw := cli(t, "-y", "volume", "wipe", "data", "--stop-tiles", "--backup-first", "--no-wait"); code != 0 ||
		writes(r)[2] != "POST "+org+`/wipe {"backup_first":true,"stop_tiles":true}` {
		t.Errorf("wipe = %d %q, wrote %v", code, errw, writes(r))
	}
	if code, _, errw := cli(t, "volume", "restore", "data"); code != 0 || writes(r)[3] != "POST "+org+"/restore-orphan " {
		t.Errorf("restore = %d %q, wrote %v", code, errw, writes(r))
	}
}

func TestShareTest(t *testing.T) {
	r := fake(t, map[string]string{"/test": `{"ok":true,"node":"n1","message":"Readable from n1.","raw":""}`})
	if code, out, errw := cli(t, "share", "test", "nas"); code != 0 || !strings.Contains(out, "Readable from n1.") || writes(r)[0] != "POST /api/v1/orgs/acme/shares/nas/test " {
		t.Errorf("readable = %d %q %q, wrote %v", code, out, errw, writes(r))
	}
	fake(t, map[string]string{"/test": `{"ok":false,"node":"n1","message":"The NAS refused access.","raw":"permission denied"}`})
	if code, out, _ := cli(t, "share", "test", "nas"); code != 1 || !strings.Contains(out, "The NAS refused access.") || !strings.Contains(out, "permission denied") {
		t.Errorf("refused = %d %q, want exit 1 with message and raw", code, out)
	}
}
