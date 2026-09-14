package repo

import "testing"

func TestValidGitURL(t *testing.T) {
	for _, u := range []string{
		"https://github.com/o/r",
		"https://github.com/o/r.git",
		"https://github.com/o/r/",
		"git@github.com:o/r.git",
		"git@github.com:o/r",
	} {
		if !ValidGitURL(u) {
			t.Errorf("want accepted: %q", u)
		}
	}
	for _, u := range []string{
		"",
		"   ",
		"/data",
		"-foo",
		"file:///data",
		"http://github.com/o/r",
		"ssh://git@github.com/o/r",
		"https://gitlab.com/o/r",
		"https://github.com/o",
		"https://github.com/o/r/extra",
		"https://user:pw@github.com/o/r",
		"git@github.com:-o/r",
		"git@github.com:o/../../etc",
		"git@gitlab.com:o/r",
	} {
		if ValidGitURL(u) {
			t.Errorf("want refused: %q", u)
		}
	}
}
