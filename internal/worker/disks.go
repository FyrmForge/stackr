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
}

// diskState is the registry and the refresh cache. Facts only reads it.
type diskState struct {
	gen  uint64
	regs []DiskReg
	ents map[string]*diskEntry // by path
}

func (s *Server) disksPath() string { return filepath.Join(s.cfg.StateDir, DisksFile) }

func (s *Server) diskEnv() hostdisk.Env { return hostdisk.Env{Root: s.cfg.HostRoot} }

// readDiskDefault is the stamp and free space of one path. Both are plain
// syscalls that can block on a dead mount: only the refresher calls it.
func (s *Server) readDiskDefault(ctx context.Context, path string) (hostdisk.Stamp, uint64, uint64, error) {
	st, err := hostdisk.ReadStamp(ctx, s.diskEnv(), path)
	var he *hostdisk.Error
	if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.As(err, &he) {
		return st, 0, 0, err
	}
	var sf syscall.Statfs_t
	if syscall.Statfs(filepath.Join(s.cfg.HostRoot, path), &sf) != nil {
		return st, 0, 0, nil
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

// replayDisks restores the saved registry at start.
func (s *Server) replayDisks() {
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
	path := e.reg.Path
	s.dmu.Unlock()
	go func() {
		rctx, cancel := context.WithTimeout(ctx, s.diskTimeout)
		defer cancel()
		st, free, total, err := s.readDisk(rctx, path)
		s.dmu.Lock()
		defer s.dmu.Unlock()
		e.running = false
		if err != nil || s.disks.ents[path] != e {
			return
		}
		e.stamp, e.free, e.total, e.at = st, free, total, time.Now()
	}()
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
			StampInstall: e.stamp.Install, Free: e.free, Total: e.total}
		for _, m := range ms { // the last entry for the point wins
			if m.Point == r.Path {
				d.Mounted, d.FSType, d.RootFS = true, m.FSType, m.Dev == rootDev
				d.Network = strings.HasPrefix(m.FSType, "nfs") || strings.Contains(" cifs smb3 smbfs ceph glusterfs 9p fuse.sshfs ", " "+m.FSType+" ")
			}
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
