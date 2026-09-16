package runtime

import (
	"io"
	"strings"
	"testing"
)

func TestDrainProgressDigest(t *testing.T) {
	d := strings.Repeat("3b79fe00", 8)
	stream := `{"status":"The push refers to repository [localhost:5000/stkr-agent]"}
{"status":"Pushed","progressDetail":{},"id":"abc"}
{"progressDetail":{},"aux":{"manifestPushedInsteadOfIndex":true}}
{"status":"latest: digest: sha256:` + d + ` size: 528"}
`
	digest, err := drainProgress(io.NopCloser(strings.NewReader(stream)), io.Discard)
	if err != nil || digest != "sha256:"+d {
		t.Fatalf("digest %q, err %v", digest, err)
	}
	_, err = drainProgress(io.NopCloser(strings.NewReader(`{"errorDetail":{"message":"denied"}}`)), io.Discard)
	if err == nil || err.Error() != "denied" {
		t.Fatalf("stream error lost: %v", err)
	}
}
