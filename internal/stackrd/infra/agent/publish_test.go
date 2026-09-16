package agent

import "testing"

func TestReleaseImage(t *testing.T) {
	for ref, want := range map[string]bool{
		"ghcr.io/fyrmforge/stackr:0.1.2":                     true,
		"ghcr.io/fyrmforge/stackr:0.1.2@sha256:512afc980cdb": true,
		"ghcr.io/fyrmforge/stackr:latest":                    false,
		"ghcr.io/fyrmforge/stackr-proxyrelay:0.1.2":          false,
		"stkr:local":          false,
		"sha256:512afc980cdb": false,
	} {
		if got := releaseImage.MatchString(ref); got != want {
			t.Errorf("%s: got %v", ref, got)
		}
	}
}
