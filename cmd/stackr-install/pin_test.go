package main

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/installspec"
)

const pinDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestPinFor(t *testing.T) {
	for _, c := range []struct {
		name, ver, own, dg, want, err string
	}{
		{"release with its digest", "0.9.1", "v0.9.1", pinDigest, pinDigest, ""},
		{"dev build, no digest", "0.9.2-dev.3", "v0.9.2-dev.3", "", "", ""},
		{"dev build asked for another release", "0.9.1", "dev", "", "", ""},
		{"release asked for another release", "0.9.0", "v0.9.1", pinDigest, "", "pins only its own"},
		{"malformed digest", "0.9.1", "v0.9.1", "sha256:zz", "", "malformed"},
	} {
		got, err := pinFor(c.ver, c.own, c.dg)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
		}
		if got != c.want {
			t.Errorf("%s: pin = %q, want %q", c.name, got, c.want)
		}
	}
}

// A local image counts as the release's only when its RepoDigests hold the
// digest under the panel repo: the tag alone, or the digest of another repo,
// does not.
func TestHasDigest(t *testing.T) {
	ref := installspec.DigestRef(pinDigest)
	for _, c := range []struct {
		name string
		have []string
		want bool
	}{
		{"the digest", []string{"other@sha256:bbb", ref}, true},
		{"another digest", []string{installspec.Repo + "@sha256:bbbb"}, false},
		{"same digest, other repo", []string{"evil.io/stackr@" + pinDigest}, false},
		{"no digests (hand-loaded)", nil, false},
	} {
		if got := hasDigest(c.have, pinDigest); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// An installer served by the panel has no digest of its own and takes the
// panel's; one that has its own ignores the flag.
func TestJoinPin(t *testing.T) {
	other := "sha256:" + strings.Repeat("b", 64)
	for _, c := range []struct{ name, embedded, given, want, err string }{
		{"served by the panel", "", pinDigest, pinDigest, ""},
		{"embedded wins", pinDigest, other, pinDigest, ""},
		{"malformed flag", "", "sha256:zz", "", "malformed"},
		{"dev, no flag", "", "", "", ""},
	} {
		got, err := joinPin("0.9.1", "v0.9.1", c.embedded, c.given)
		if got != c.want || (err == nil) != (c.err == "") || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: %q %v", c.name, got, err)
		}
	}
}
