package main

import (
	"strings"
	"testing"
)

// route add posts the flags as the body; --to and --mode are required.
func TestRouteAdd(t *testing.T) {
	r := &recorder{}
	r.serve(t)
	code, _, errw := cli(t, strings.Fields("admin route add pve.io --to 10.0.0.5:8006 --mode https --insecure")...)
	if code != 0 {
		t.Fatalf("route add = %d %s", code, errw)
	}
	want := `POST /api/v1/admin/routes {"host":"pve.io","insecure":true,"mode":"https","target":"10.0.0.5:8006"}`
	if len(r.reqs) == 0 || r.reqs[len(r.reqs)-1] != want {
		t.Errorf("sent %q, want last %q", r.reqs, want)
	}
	if code, _, _ := cli(t, strings.Fields("admin route add pve.io --mode https")...); code == 0 {
		t.Error("route add without --to succeeded")
	}
}
