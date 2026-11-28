package deploy

import (
	"errors"
	"io"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
)

// deniedErr is the plain sentence first, the daemon's own text underneath.
type deniedErr struct {
	msg string
	err error
}

func (e deniedErr) Error() string { return e.msg }
func (e deniedErr) Unwrap() error { return e.err }

// shareDenied turns a daemon "permission denied" on one of r's share volumes
// into the plain NFS text (kept in the deploy log too). Any other error is
// returned unchanged.
func (r resolved) shareDenied(err error, log io.Writer) error {
	var md docker.MountDeniedError
	if !errors.As(err, &md) {
		return err
	}
	s, ok := r.shareVols[md.Volume]
	if !ok {
		return err
	}
	msg := volume.ShareDenied(s) + "\n" + volume.MaskMount(md.Raw)
	logf(log, "%s\n", msg)
	return deniedErr{msg: msg, err: err}
}
