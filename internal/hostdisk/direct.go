package hostdisk

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"syscall"
	"unsafe"
)

// ioErrnos are the failures that say a disk is erroring: a dead or shut down
// filesystem, not a full one. ENOSPC and EDQUOT are never among them.
var ioErrnos = []syscall.Errno{syscall.EIO, syscall.EUCLEAN, syscall.ENODEV, syscall.ENXIO, syscall.ESTALE, syscall.EROFS}

// IOFailure reports whether err is a read error of an erroring disk. It sees
// through *Error, which keeps only the text of the errno it came from.
func IOFailure(err error) bool {
	if err == nil {
		return false
	}
	var he *Error
	msg := ""
	if errors.As(err, &he) {
		if he.Code != CodeIO {
			return false
		}
		msg = he.Message
	}
	for _, e := range ioErrnos {
		if errors.Is(err, e) || (msg != "" && strings.Contains(msg, e.Error())) {
			return true
		}
	}
	return false
}

// ReadDirect reads the stamp of the disk at path with O_DIRECT, so the answer
// comes from the device and not from the page cache. A disk with no stamp, or
// a filesystem that refuses O_DIRECT, is no error. It writes nothing.
func ReadDirect(ctx context.Context, e Env, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t, herr := e.open(path, CodeDiskMissing)
	if herr != nil {
		return herr
	}
	defer t.close()
	f, err := t.root.OpenFile(stampFile, syscall.O_RDONLY|syscall.O_DIRECT, 0)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.EINVAL):
		return nil
	default:
		return err
	}
	defer func() { _ = f.Close() }()
	// O_DIRECT wants an aligned buffer and a block-sized read
	raw := make([]byte, 2*4096)
	off := int((4096 - uintptr(unsafe.Pointer(&raw[0]))%4096) % 4096)
	if _, err := f.Read(raw[off : off+4096]); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}
