package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	bk "github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Point-in-time restore, the flow's half: fill a fresh data volume with the
// base backup, the WAL bundles to replay and the recovery settings, all in
// one stream the volume helper unpacks. The decrypt keys never leave the
// panel: the helper container only ever sees a plain tar.

// Hold marks the archive under root as being read by a restore: PruneWindow
// leaves it alone until release. Held counts, so overlapping restores nest.
func (f *Flow) Hold(root string) (release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holds == nil {
		f.holds = map[string]int{}
	}
	f.holds[root]++
	var once sync.Once // a restore releases early and again on its way out
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.holds[root]--; f.holds[root] <= 0 {
				delete(f.holds, root)
			}
		})
	}
}

func (f *Flow) held(root string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.holds[root] > 0
}

// Replay is one point-in-time restore into a fresh volume.
type Replay struct {
	Dest   store.BackupDest // with its archive key
	Plan   bk.Plan
	Target store.Volume // the new instance's data volume, empty
	// WalDir is where the bundles' files go, relative to the data directory;
	// Conf is appended to its postgresql.auto.conf (restore_command and the
	// recovery target, built by the engine).
	WalDir string
	Conf   string
}

// FillVolume unpacks the base into the target volume, adds the WAL files
// under WalDir, an empty recovery.signal and Conf. It wipes the volume
// first, as every untar does: the target is new.
func (f *Flow) FillVolume(ctx context.Context, r Replay, log io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	logf(log, "unpacking %s and %d WAL bundle(s) into %s\n", path.Base(r.Plan.Base.ObjectKey), len(r.Plan.Bundles), r.Target.Slug)
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(f.replayTar(ctx, r, pw)) }()
	err := f.Volumes.Untar(ctx, r.Target, pr)
	_ = pr.CloseWithError(err) // an untar that stopped early must not strand the producer
	return err
}

// object opens a decrypted, gunzipped tar of the archive object at key.
func (f *Flow) object(ctx context.Context, d store.BackupDest, key string) (*tar.Reader, func(), error) {
	obj, err := open(d).Get(ctx, key)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s: %w", key, err)
	}
	dec, err := decrypt(obj, d.ArchiveKey)
	if err != nil {
		_ = obj.Close()
		return nil, nil, fmt.Errorf("decrypt %s: %w", key, err)
	}
	gz, err := gzip.NewReader(dec)
	if err != nil {
		_ = obj.Close()
		return nil, nil, fmt.Errorf("%s is not a gzip archive: %w", key, err)
	}
	return tar.NewReader(gz), func() { _ = obj.Close() }, nil
}

const autoConf = "postgresql.auto.conf"

// replayTar writes the volume's whole content as a gzipped tar to w.
func (f *Flow) replayTar(ctx context.Context, r Replay, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	base, done, err := f.object(ctx, r.Dest, r.Plan.Base.ObjectKey)
	if err != nil {
		return err
	}
	defer done()
	uid, gid, haveConf := 0, 0, false
	for {
		h, err := base.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read the base backup: %w", err)
		}
		name := path.Clean(h.Name)
		switch name {
		case "recovery.signal", "standby.signal":
			continue // never ours to inherit
		case "PG_VERSION":
			uid, gid = h.Uid, h.Gid
		case autoConf:
			haveConf = true
			old, err := io.ReadAll(base)
			if err != nil {
				return err
			}
			body := append(bytes.TrimRight(old, "\n"), []byte("\n"+r.Conf)...)
			h.Size = int64(len(body))
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			if _, err := tw.Write(body); err != nil {
				return err
			}
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, base); err != nil {
			return fmt.Errorf("read the base backup: %w", err)
		}
	}
	if !haveConf {
		if err := writeFile(tw, autoConf, []byte(r.Conf), uid, gid); err != nil {
			return err
		}
	}
	if err := writeFile(tw, "recovery.signal", nil, uid, gid); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: r.WalDir + "/", Mode: 0o700, Uid: uid, Gid: gid}); err != nil {
		return err
	}
	for _, b := range r.Plan.Bundles {
		if err := f.copyBundle(ctx, r, b, tw, uid, gid); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeFile(tw *tar.Writer, name string, body []byte, uid, gid int) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: int64(len(body)), Uid: uid, Gid: gid}); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}

// copyBundle puts a bundle's WAL files under WalDir. Only WAL names pass, so
// nothing an archive holds can land anywhere else in the volume.
func (f *Flow) copyBundle(ctx context.Context, r Replay, b bk.Bundle, tw *tar.Writer, uid, gid int) error {
	tr, done, err := f.object(ctx, r.Dest, b.Key)
	if err != nil {
		return err
	}
	defer done()
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", b.Key, err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag != tar.TypeReg || name != path.Base(name) || !bk.WALName(name) {
			return fmt.Errorf("%s holds %q, which is not a WAL file", b.Key, h.Name)
		}
		hh := &tar.Header{Typeflag: tar.TypeReg, Name: r.WalDir + "/" + name, Mode: 0o600, Size: h.Size, Uid: uid, Gid: gid}
		if err := tw.WriteHeader(hh); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return fmt.Errorf("read %s: %w", b.Key, err)
		}
	}
}

// ListArchive is the archive's base runs and WAL keys under root: what a
// restore plans from.
func (f *Flow) ListArchive(ctx context.Context, d store.BackupDest, root string) (bases []store.BackupRun, walKeys []string, err error) {
	if bases, err = f.Backups.Bases(ctx, root); err != nil {
		return nil, nil, err
	}
	if walKeys, err = open(d).List(ctx, root+"/"+bk.WALDir+"/"); err != nil {
		return nil, nil, err
	}
	return bases, walKeys, nil
}
