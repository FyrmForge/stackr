package runtime

import (
	"io"
	"strings"
	"testing"
)

func TestDrainProgressDigest(t *testing.T) {
	stream := `{"status":"The push refers to repository [localhost:5000/stkr-agent]"}
{"status":"Pushed","progressDetail":{},"id":"abc"}
{"status":"latest: digest: sha256:3b79fe size: 528"}
{"progressDetail":{},"aux":{"Tag":"latest","Digest":"sha256:3b79fe","Size":528}}
`
	digest, err := drainProgress(io.NopCloser(strings.NewReader(stream)), io.Discard)
	if err != nil || digest != "sha256:3b79fe" {
		t.Fatalf("digest %q, err %v", digest, err)
	}
	_, err = drainProgress(io.NopCloser(strings.NewReader(`{"errorDetail":{"message":"denied"}}`)), io.Discard)
	if err == nil || err.Error() != "denied" {
		t.Fatalf("stream error lost: %v", err)
	}
}
