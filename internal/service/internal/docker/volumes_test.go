package docker

import (
	"errors"
	"fmt"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
)

func TestIsVolumeInUse(t *testing.T) {
	for err, want := range map[error]bool{
		fmt.Errorf("x: %w", cerrdefs.ErrConflict):         true,
		errors.New("remove v: volume is in use - [abc]"):  true,
		errors.New("Cannot connect to the Docker daemon"): false,
		fmt.Errorf("x: %w", cerrdefs.ErrNotFound):         false,
		nil: false,
	} {
		if IsVolumeInUse(err) != want {
			t.Errorf("IsVolumeInUse(%v) = %v", err, !want)
		}
	}
}

// An empty label value filters on "key=", never on the key alone: a blank id
// must not match every container.
func TestLabelFilterEmptyValue(t *testing.T) {
	f := labelFilter(map[string]string{"stackr.tile": ""})
	if !f.ExactMatch("label", "stackr.tile=") || f.ExactMatch("label", "stackr.tile") {
		t.Errorf("filter = %v", f.Get("label"))
	}
}
