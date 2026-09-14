package backup

import "syscall"

// freeBytes reports the space available to a non-root user on the filesystem
// holding dir. Zero means "could not tell", the caller treats that as no gate
// rather than as a failure.
func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
