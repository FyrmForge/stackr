package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/FyrmForge/stackr/internal/hostdisk"
)

// DisksFile in the state dir is the registry the controller last pushed,
// replayed at start.
const DisksFile = "disks.json"

const (
	diskEvery   = 30 * time.Second // refresh interval per path
	diskTimeout = 10 * time.Second // a refresh past this marks its path stale
)

// DiskReg is one registered disk: the controller pushes the full list with
// PUT /v1/disks, and the worker reports on exactly these paths.
type DiskReg struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Owner string `json:"owner"`
	// Device is the Device.ID behind an xfs disk; "" for a path disk and from
	// an older panel, which then gets no device facts.
	Device string `json:"device,omitempty"`
}

type disksFile struct {
	Generation uint64    `json:"generation"`
	Disks      []DiskReg `json:"disks"`
}

// diskEntry is one registered path and what its refresher last read.
type diskEntry struct {
	reg     DiskReg
	cancel  context.CancelFunc
	running bool      // a read is in flight (single flight per path)
	started time.Time // when it began
	at      time.Time // last successful read, zero = never
	stamp   hostdisk.Stamp
	free    uint64
	total   uint64
	devSize int64 // xfs: the device size and filesystem uuid, read with the rest
	fsuuid  string
	devGone bool // the last read failed and the mounted device is gone from sysfs
	ioErr   bool // the last read ended in an I/O error; stamp keeps the last good read
}

// diskState is the registry and the refresh cache. Facts only reads it.
type diskState struct {
	gen  uint64
	regs []DiskReg
	ents map[string]*diskEntry // by path
}

func (s *Server) disksPath() string { return filepath.Join(s.cfg.StateDir, DisksFile) }

func (s *Server) diskEnv() hostdisk.Env {
	return hostdisk.Env{Root: s.cfg.HostRoot, TempMounts: s.diskTemp}
}

// readDiskDefault is the stamp and free space of one path. Both are plain
// syscalls that can block on a dead mount: only the refresher calls it.
// Only a real answer is a read: a stamp, no stamp on a mounted path, or the
// path not being the disk at all (disk_missing). Anything else (an I/O error,
// a broken stamp, no free space figure) is an error, so the path goes stale and latches nothing.
func (s *Server) readDiskDefault(ctx context.Context, path string) (hostdisk.Stamp, uint64, uint64, error) {
	st, err := hostdisk.ReadStamp(ctx, s.diskEnv(), path)
	var he *hostdisk.Error
	switch {
	case err == nil, errors.Is(err, fs.ErrNotExist):
	case errors.As(err, &he) && he.Code == hostdisk.CodeDiskMissing:
		return hostdisk.Stamp{}, 0, 0, nil
	default:
		return st, 0, 0, err
	}
	// the stamp again, straight from the device: the page cache cannot hide a dead disk
	if err := hostdisk.ReadDirect(ctx, s.diskEnv(), path); err != nil {
		var he *hostdisk.Error
		if !errors.As(err, &he) || he.Code != hostdisk.CodeDiskMissing {
			return st, 0, 0, err
		}
	}
	var sf syscall.Statfs_t
	if err := syscall.Statfs(filepath.Join(s.cfg.HostRoot, path), &sf); err != nil {
		return st, 0, 0, err // no sizes is no read: the entry goes stale
	}
	return st, uint64(sf.Bavail) * uint64(sf.Bsize), uint64(sf.Blocks) * uint64(sf.Bsize), nil
}

func validDisks(regs []DiskReg) error {
	ids, paths := map[string]bool{}, map[string]bool{}
	for _, r := range regs {
		switch {
		case r.ID == "" || r.Owner == "":
			return errors.New("disk id and owner are required")
		case !filepath.IsAbs(r.Path) || filepath.Clean(r.Path) != r.Path:
			return fmt.Errorf("disk path %q is not a clean absolute path", r.Path)
		case ids[r.ID] || paths[r.Path]:
			return fmt.Errorf("disk %q is listed twice", r.ID)
		}
		ids[r.ID], paths[r.Path] = true, true
	}
	return nil
}

// PutDisks replaces the registry: validate, persist, then swap in memory and
// bump the generation, then start and stop refreshers. A failed persist
// changes nothing in memory.
func (s *Server) PutDisks(regs []DiskReg) error {
	if err := validDisks(regs); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dmu.Lock()
	gen := s.disks.gen + 1
	s.dmu.Unlock()
	b, _ := json.Marshal(disksFile{Generation: gen, Disks: regs})
	if err := writeAtomic(s.disksPath(), b); err != nil {
		return err
	}
	s.setDisks(gen, regs)
	return nil
}

// setDisks swaps the registry in. A kept path keeps its entry and refresher.
func (s *Server) setDisks(gen uint64, regs []DiskReg) {
	s.dmu.Lock()
	defer s.dmu.Unlock()
	next := map[string]*diskEntry{}
	var fresh []*diskEntry
	for _, r := range regs {
		if e := s.disks.ents[r.Path]; e != nil {
			e.reg = r
			next[r.Path] = e
			continue
		}
		e := &diskEntry{reg: r}
		next[r.Path] = e
		fresh = append(fresh, e)
	}
	for p, e := range s.disks.ents {
		if next[p] == nil {
			e.cancel()
		}
	}
	s.disks.gen, s.disks.regs, s.disks.ents = gen, append([]DiskReg(nil), regs...), next
	for _, e := range fresh {
		ctx, cancel := context.WithCancel(s.ctx)
		e.cancel = cancel
		go s.diskLoop(ctx, e)
	}
}

// ReplayDisks restores the saved registry at start.
func (s *Server) ReplayDisks() {
	b, err := os.ReadFile(s.disksPath())
	if err != nil {
		return
	}
	var f disksFile
	if json.Unmarshal(b, &f) != nil || validDisks(f.Disks) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setDisks(f.Generation, f.Disks)
}

func (s *Server) diskLoop(ctx context.Context, e *diskEntry) {
	t := time.NewTicker(s.diskEvery)
	defer t.Stop()
	for {
		s.refreshDisk(ctx, e)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refreshDisk starts one read of e's path unless one is already in flight. The
// loop never waits for it: a read stuck in the kernel costs this one goroutine
// and no new one until it returns. The result is dropped when e left the
// registry meanwhile (a removed or re-added path).
func (s *Server) refreshDisk(ctx context.Context, e *diskEntry) {
	s.dmu.Lock()
	if e.running || ctx.Err() != nil {
		s.dmu.Unlock()
		return
	}
	e.running, e.started = true, time.Now()
	path, dev := e.reg.Path, e.reg.Device
	s.dmu.Unlock()
	go func() {
		rctx, cancel := context.WithTimeout(ctx, s.diskTimeout)
		defer cancel()
		st, free, total, err := s.readDisk(rctx, path)
		s.dmu.Lock()
		defer s.dmu.Unlock()
		e.running = false
		if s.disks.ents[path] != e {
			return
		}
		if err != nil {
			// an I/O error is a real answer: the disk is erroring. Anything else
			// leaves the entry to go stale and latches nothing.
			if hostdisk.IOFailure(err) {
				e.ioErr, e.at = true, time.Now()
				e.devGone = dev != "" && s.mountGone(path)
			}
			return
		}
		e.ioErr, e.devGone = false, false
		e.stamp, e.free, e.total, e.at = st, free, total, time.Now()
		if dev != "" { // sysfs reads, only here and never on a request
			// The device is the one the mount stands on, not the recorded name: with
			// no by-id link a kernel name can be another disk after a reboot.
			e.devSize, e.fsuuid = 0, ""
			if md := mountDev(s.cfg.HostRoot, path); md != "" {
				if sz, u, derr := hostdisk.ReadMounted(s.cfg.HostRoot, md); derr == nil {
					e.devSize, e.fsuuid = sz, u
				}
			}
		}
	}()
}

// devGone reports whether the block device major:minor left the host's sysfs.
// With no readable sys/dev/block (the panel container has no host sysfs bind)
// it answers false: erroring, never missing.
func devGone(root, dev string) bool {
	if root == "" {
		root = "/"
	}
	dir := filepath.Join(root, "sys/dev/block")
	if _, err := os.Lstat(dir); err != nil {
		return false
	}
	_, err := os.Lstat(filepath.Join(dir, dev))
	return errors.Is(err, fs.ErrNotExist)
}

// mountGone: path is still in the mount table but its device is not in sysfs.
func (s *Server) mountGone(path string) bool {
	md := mountDev(s.cfg.HostRoot, path)
	return md != "" && devGone(s.cfg.HostRoot, md)
}

// mountDev is the major:minor of the device the host has mounted at path ("" when nothing is).
func mountDev(root, path string) string {
	ms := hostdisk.ParseMounts([]byte(readFile(strings.TrimSuffix(root, "/") + "/proc/1/mountinfo")))
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Point == path {
			return ms[i].Dev
		}
	}
	return ""
}

// diskFacts fills the disk fields of f. It parses the mount table and serves
// the cache; it touches no registered filesystem.
func (s *Server) diskFacts(f *Facts) {
	f.DisksRead = true
	s.dmu.Lock()
	regs := append([]DiskReg(nil), s.disks.regs...)
	ents := make(map[string]diskEntry, len(regs))
	for p, e := range s.disks.ents {
		ents[p] = *e
	}
	s.dmu.Unlock()
	f.DiskRegs = regs
	f.Disks = []hostdisk.Fact{}
	if len(regs) == 0 {
		return
	}
	ms := hostdisk.ParseMounts([]byte(readFile(strings.TrimSuffix(s.cfg.HostRoot, "/") + "/proc/1/mountinfo")))
	var rootDev string
	for _, m := range ms {
		if m.Point == "/" {
			rootDev = m.Dev
		}
	}
	now := time.Now()
	for _, r := range regs {
		e := ents[r.Path]
		d := hostdisk.Fact{Path: r.Path, StampID: e.stamp.ID, StampOwner: e.stamp.Owner,
			StampInstall: e.stamp.Install, Free: e.free, Total: e.total, ReadAt: e.at,
			Device: r.Device, DeviceBytes: e.devSize, FSUUID: e.fsuuid, IOErr: e.ioErr, DeviceGone: e.devGone}
		for _, m := range ms { // the last entry for the point wins
			if m.Point == r.Path {
				d.Mounted, d.FSType, d.RootFS, d.ReadOnly = true, m.FSType, m.Dev == rootDev, m.RO
				d.Network = hostdisk.IsNetwork(m.FSType)
			}
		}
		// A read stuck in the kernel for 3 intervals is a dead disk, not a stale
		// cache: report it erroring so the controller's latch can fire.
		// Only a disk with a device: a path or network disk (a hard-mounted NFS
		// outage hangs reads) stays stale. ReadAt advances once per interval so
		// the controller's distinct-reads debounce still counts.
		if e.running && r.Device != "" && !d.Network && now.Sub(e.started) > 3*s.diskEvery {
			d.IOErr = true
			d.ReadAt = e.started.Add(now.Sub(e.started).Truncate(s.diskEvery))
			d.DeviceGone = r.Device != "" && s.mountGone(r.Path)
			f.Disks = append(f.Disks, d)
			continue
		}
		f.Disks = append(f.Disks, d)
		if e.at.IsZero() || now.Sub(e.at) > 2*s.diskEvery || (e.running && now.Sub(e.started) > s.diskTimeout) {
			f.DisksStale = true
		}
	}
}

func (s *Server) putDisks(w http.ResponseWriter, r *http.Request) {
	b, ok := readBody(w, r)
	if !ok {
		return
	}
	var regs []DiskReg
	if err := json.Unmarshal(b, &regs); err != nil {
		fail(w, 400, err)
		return
	}
	if err := s.PutDisks(regs); err != nil {
		code := 500
		if validDisks(regs) != nil {
			code = 400
		}
		fail(w, code, err)
		return
	}
	w.WriteHeader(204)
}
