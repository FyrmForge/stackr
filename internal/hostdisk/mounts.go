package hostdisk

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Mount is one line of a mountinfo table.
type Mount struct {
	Dev    string // major:minor
	Root   string
	Point  string
	FSType string
	Source string
	RO     bool // mounted read-only
}

// ParseMounts reads a mountinfo table. It takes bytes only and touches no
// filesystem. Lines it cannot read are skipped.
func ParseMounts(table []byte) []Mount {
	var out []Mount
	for _, line := range bytes.Split(table, []byte("\n")) {
		f := strings.Fields(string(line))
		// id parent dev root point opts [optional...] - fstype source superopts
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || len(f) < sep+3 {
			continue
		}
		out = append(out, Mount{Dev: f[2], Root: unescape(f[3]), Point: unescape(f[4]), FSType: f[sep+1], Source: unescape(f[sep+2]), RO: slices.Contains(strings.Split(f[5], ","), "ro")})
	}
	return out
}

// unescape decodes the octal escapes (\040) the kernel writes for spaces,
// tabs, newlines and backslashes.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// lastCovering is the mount that covers path: the last table entry whose
// point is path or a parent of it (a later mount hides an earlier one).
func lastCovering(ms []Mount, path string) (Mount, bool) {
	for i := len(ms) - 1; i >= 0; i-- {
		p := ms[i].Point
		if p == path || p == "/" || strings.HasPrefix(path, p+"/") {
			return ms[i], true
		}
	}
	return Mount{}, false
}

// IsNetwork reports a filesystem that lives across a network.
func IsNetwork(fs string) bool {
	switch {
	case strings.HasPrefix(fs, "nfs"), fs == "cifs", fs == "smb3", fs == "smbfs", fs == "ceph",
		fs == "glusterfs", fs == "9p", fs == "fuse.sshfs":
		return true
	}
	return false
}

// resolveUnder resolves symlinks in p with root as the filesystem root, so
// an absolute link inside root never leaves it. It returns the canonical
// absolute path as the host sees it ("/mnt/disk").
func resolveUnder(root, p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", errors.New("path is not absolute")
	}
	rest := strings.Split(filepath.Clean(p), "/")
	var cur []string // resolved components
	links := 0
	for len(rest) > 0 {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(cur) > 0 {
				cur = cur[:len(cur)-1]
			}
			continue
		}
		full := filepath.Join(root, "/"+strings.Join(append(cur, c), "/"))
		fi, err := os.Lstat(full)
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = append(cur, c)
			continue
		}
		if links++; links > 40 {
			return "", &fs.PathError{Op: "resolve", Path: p, Err: syscall.ELOOP}
		}
		dst, err := os.Readlink(full)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(dst) {
			cur = nil
		}
		rest = append(strings.Split(dst, "/"), rest...)
	}
	return "/" + strings.Join(cur, "/"), nil
}

// ReadMounted is ReadDevice for the block device with this major:minor (a
// mountinfo Dev), found through /sys/dev/block. The mount's name column is
// never used: a kernel name can belong to another disk by now.
func ReadMounted(root, majmin string) (size int64, fsuuid string, err error) {
	dst, err := os.Readlink(joinRoot(root, "/sys/dev/block/"+majmin))
	if err != nil {
		return 0, "", err
	}
	return ReadDevice(root, "/dev/"+filepath.Base(dst))
}

// ReadDevice is the size and filesystem uuid of the block device a Device.ID
// (a /dev/disk/by-id link or a kernel path) names, from sysfs and the by-uuid
// links under root. It runs no command. The uuid is "" when no link points at
// the device.
func ReadDevice(root, id string) (size int64, fsuuid string, err error) {
	kname := filepath.Base(id)
	if dst, err := os.Readlink(joinRoot(root, id)); err == nil { // a by-id link names the kernel node
		kname = filepath.Base(dst)
	}
	b, err := os.ReadFile(joinRoot(root, "/sys/class/block/"+kname+"/size"))
	if err != nil {
		return 0, "", err
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, "", err
	}
	dir := joinRoot(root, "/dev/disk/by-uuid")
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if dst, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && filepath.Base(dst) == kname {
			return sectors * 512, e.Name(), nil
		}
	}
	return sectors * 512, "", nil
}
