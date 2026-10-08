package main

import (
	"strings"
	"testing"
)

// approve names what it grants and asks first; with nothing waiting it
// sends nothing.
func TestHostGrantApprove(t *testing.T) {
	const path = "/api/v1/orgs/acme/stacks/shop/host-grant"
	r := fake(t, map[string]string{"": `{"granted":false,"lines":[],"pending":[]}`})
	if code, _, errw := cli(t, "-y", "host-grant", "approve", "--stack", "shop"); code == 0 || !strings.Contains(errw, "nothing waits") || len(writes(r)) != 0 {
		t.Errorf("nothing waiting = %d %q, writes %v", code, errw, writes(r))
	}

	r = fake(t, map[string]string{"": `{"granted":false,"lines":[],"pending":["web host:/a:/b","web privileged"]}`})
	code, _, errw := cli(t, "host-grant", "approve", "--stack", "shop")
	if code == 0 || !strings.Contains(errw, "refusing without --yes") || !strings.Contains(errw, "web host:/a:/b, web privileged") || len(writes(r)) != 0 {
		t.Errorf("without a yes = %d %q, writes %v", code, errw, writes(r))
	}
	if code, _, errw = cli(t, "-y", "host-grant", "approve", "--stack", "shop"); code != 0 {
		t.Fatalf("-y = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "POST "+path+`/approve {"pending":["web host:/a:/b","web privileged"]}` {
		t.Errorf("writes = %v", w)
	}

	// --only narrows: it sends the shown set and the part to grant, and a
	// line that is not asked is refused before anything is sent.
	r = fake(t, map[string]string{"": `{"granted":false,"lines":[],"pending":["web host:/a:/b","web privileged"]}`})
	if code, _, errw = cli(t, "-y", "host-grant", "approve", "--stack", "shop", "--only", "web privileged"); code != 0 {
		t.Fatalf("--only = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "POST "+path+`/approve {"grant":["web privileged"],"pending":["web host:/a:/b","web privileged"]}` {
		t.Errorf("writes = %v", w)
	}
	r = fake(t, map[string]string{"": `{"granted":false,"lines":[],"pending":["web privileged"]}`})
	if code, _, errw = cli(t, "-y", "host-grant", "approve", "--stack", "shop", "--only", "db privileged"); code == 0 || len(writes(r)) != 0 {
		t.Errorf("--only of an unasked line = %d %q, writes %v", code, errw, writes(r))
	}
}

// revoke asks first and sends one DELETE.
func TestHostGrantRevoke(t *testing.T) {
	r := fake(t, map[string]string{"": `{}`})
	if code, _, errw := cli(t, "host-grant", "revoke", "--stack", "shop"); code == 0 || !strings.Contains(errw, "refusing without --yes") || len(writes(r)) != 0 {
		t.Errorf("without a yes = %d %q, writes %v", code, errw, writes(r))
	}
	if code, _, errw := cli(t, "-y", "host-grant", "revoke", "--stack", "shop"); code != 0 {
		t.Fatalf("-y = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || !strings.HasPrefix(w[0], "DELETE /api/v1/orgs/acme/stacks/shop/host-grant") {
		t.Errorf("writes = %v", w)
	}
	r = fake(t, map[string]string{"": `{}`})
	if code, _, errw := cli(t, "-y", "host-grant", "revoke", "--stack", "shop", "--tile", "web"); code != 0 {
		t.Fatalf("--tile = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || !strings.Contains(w[0], "host-grant?tile=web") {
		t.Errorf("tile revoke writes = %v", w)
	}
}
