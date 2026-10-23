package service

import "testing"

// A connection made after the panel moved registers the new host with its
// provider (the manifest's hook and setup_url, the webhook sync): the
// panel_domain setting swaps BaseURL's host, keeping scheme and port.
func TestAppBaseFollowsPanelHost(t *testing.T) {
	for _, c := range []struct{ base, panel, want string }{
		{"https://old.example.com", "new.example.com", "https://new.example.com"},
		{"https://old.example.com:8443/", "new.example.com", "https://new.example.com:8443"},
		{"https://old.example.com/", "", "https://old.example.com"},
	} {
		if got := withHost(c.base, c.panel); got != c.want {
			t.Errorf("%s + %q = %s, want %s", c.base, c.panel, got, c.want)
		}
	}
}
