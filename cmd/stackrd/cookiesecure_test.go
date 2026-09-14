package main

import "testing"

// The bug this covers: STACKR_TLS=off with DEV_MODE unset, which is what
// install.sh produces for localhost, a bare IP, or any privately-reachable
// name. That served Secure cookies over plain HTTP, and the browser dropped
// every one of them.
func TestCookieSecureFollowsTLSNotDevMode(t *testing.T) {
	cases := []struct {
		name    string
		devMode bool
		tlsOff  bool
		want    bool
	}{
		{"public install, TLS on", false, false, true},
		{"LAN install, TLS off", false, true, false},
		{"local dev over http", true, false, false},
		{"dev mode and TLS off", true, true, false},
	}
	for _, c := range cases {
		if got := cookieSecureFor(c.devMode, c.tlsOff); got != c.want {
			t.Errorf("%s: cookieSecureFor(devMode=%v, tlsOff=%v) = %v, want %v",
				c.name, c.devMode, c.tlsOff, got, c.want)
		}
	}
}
