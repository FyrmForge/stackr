package service

import "testing"

// Behind NAT the controller's DNS client IP is the router's, as an outside
// worker sees it; unseen (or IPv6) it stays empty for the admin to type.
func TestNATIPv4(t *testing.T) {
	m := ClusterMap{
		Nodes:    []ClusterNode{{ID: "ctl", Net: "home"}, {ID: "w1", Net: "hetzner"}},
		Networks: []ClusterNet{{ID: "home", Router: true, Public: "198.51.100.9"}, {ID: "hetzner"}},
	}
	if got := natIPv4(m, "ctl"); got != "198.51.100.9" {
		t.Errorf("seen router = %q", got)
	}
	m.Networks[0].Public = ""
	if got := natIPv4(m, "ctl"); got != "" {
		t.Errorf("unseen router = %q, want empty", got)
	}
	m.Networks[0].Public = "2001:db8::1"
	if got := natIPv4(m, "ctl"); got != "" {
		t.Errorf("IPv6 public = %q, want empty", got)
	}
	if got := natIPv4(m, "nope"); got != "" {
		t.Errorf("unknown node = %q", got)
	}
}
