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

func TestDeviceOf(t *testing.T) {
	for name, c := range map[string]struct {
		opts map[string]string
		want string
	}{
		"plain":    {nil, ""},
		"bound":    {BindOpts("/mnt/d/v"), "/mnt/d/v"},
		"bind+opt": {map[string]string{"type": "none", "o": "rbind,bind", "device": "/x"}, "/x"},
		"tmpfs":    {map[string]string{"type": "tmpfs", "o": "size=1m", "device": "tmpfs"}, ""},
		"nfs":      {map[string]string{"type": "nfs", "o": "addr=h", "device": ":/p"}, ""},
	} {
		if got := DeviceOf(c.opts); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}
