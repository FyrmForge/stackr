package main

import (
	"strings"
	"testing"
)

func TestNodeDiskMoveObserve(t *testing.T) {
	plan := `{"node_id":"n1","back":false,"size_bytes":1073741824,"cap":2147483648,"need":1181116006,"free":53687091200,"fits":true,"busy":false}`
	r := fake(t, map[string]string{"/move-observe-plan": plan, "/move-observe": `{"id":"","kind":""}`})
	if code, out, _ := cli(t, "node", "disk", "move-observe", "n1"); code != 3 || !strings.Contains(out, "cap on the disk") || len(writes(r)) != 0 {
		t.Errorf("unconfirmed = %d %q, wrote %v", code, out, writes(r))
	}
	if code, _, errw := cli(t, "-y", "node", "disk", "move-observe", "n1", "--back", "--no-wait"); code != 0 || len(writes(r)) != 1 || writes(r)[0] != "POST "+nd+"/move-observe?back=true " {
		t.Errorf("back = %d %q, wrote %v", code, errw, writes(r))
	}
	fake(t, map[string]string{"/move-observe-plan": strings.Replace(plan, `"fits":true`, `"fits":false`, 1)})
	if code, _, errw := cli(t, "-y", "node", "disk", "move-observe", "n1"); code != 1 || !strings.Contains(errw, "need") {
		t.Errorf("does not fit = %d %q", code, errw)
	}
}
