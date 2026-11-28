package deploy

import (
	"errors"
	"io"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// deniedErr is the plain sentence first, the daemon's own text underneath.
type deniedErr struct {
	msg string
	err error
}

func (e deniedErr) Error() string { return e.msg }
func (e deniedErr) Unwrap() error { return e.err }

// shareDenied turns a daemon "permission denied" on one of r's share volumes
// into the plain NFS text (kept in the deploy log too). With a share volume in
// play, any other error is masked too: the daemon can echo the mount options,
// login included, on any failure. The original stays wrapped (errors.As).
func (r resolved) shareDenied(err error, log io.Writer) error {
	if len(r.shareVols) == 0 || err == nil {
		return err
	}
	var md docker.MountDeniedError
	var s store.Share
	ok := false
	if errors.As(err, &md) {
		s, ok = r.shareVols[md.Volume]
	}
	if !ok {
		if m := volume.MaskMount(err.Error()); m != err.Error() {
			return deniedErr{msg: m, err: err}
		}
		return err
	}
	msg := volume.ShareDenied(s) + "\n" + volume.MaskMount(md.Raw)
	logf(log, "%s\n", msg)
	return deniedErr{msg: msg, err: err}
}
