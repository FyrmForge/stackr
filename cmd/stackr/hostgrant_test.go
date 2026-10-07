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

	r = fake(t, map[string]string{"": `{"granted":false,"lines":[],"pending":["host:/a:/b","privileged"]}`})
	code, _, errw := cli(t, "host-grant", "approve", "--stack", "shop")
	if code == 0 || !strings.Contains(errw, "refusing without --yes") || !strings.Contains(errw, "host:/a:/b, privileged") || len(writes(r)) != 0 {
		t.Errorf("without a yes = %d %q, writes %v", code, errw, writes(r))
	}
	if code, _, errw = cli(t, "-y", "host-grant", "approve", "--stack", "shop"); code != 0 {
		t.Fatalf("-y = %d %q", code, errw)
	}
	if w := writes(r); len(w) != 1 || w[0] != "POST "+path+`/approve {"pending":["host:/a:/b","privileged"]}` {
		t.Errorf("writes = %v", w)
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
}
