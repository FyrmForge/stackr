package panel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/FyrmForge/stackr/internal/hostdisk"
	"github.com/FyrmForge/stackr/internal/paneldb"
)

// dbFiles is how an ordinary Update copies the database aside and puts it back.
// On an unmoved install the helper does it itself; on one whose database is on a
// node disk every file operation under db/ is the disk helper's.
type dbFiles interface {
	Save(ctx context.Context, db, cp string) error
	Restore(ctx context.Context, db, cp string) error
	Recover(ctx context.Context, dataDir string) error
}

type localFiles struct{}

func (localFiles) Save(_ context.Context, db, cp string) error    { return paneldb.SaveCopy(db, cp) }
func (localFiles) Restore(_ context.Context, db, cp string) error { return paneldb.RestoreCopy(db, cp) }
func (localFiles) Recover(_ context.Context, dataDir string) error {
	return paneldb.Recover(dataDir)
}

// diskFiles is the moved install's version. The copy is a copy (never a
// rename: swap/ and the disk are different filesystems, EXDEV), taken after a
// TRUNCATE checkpoint so the main file alone holds every row, and checked by
// hash. Restore removes the live -wal and -shm first, then copies over.
type diskFiles struct{ m *mover }

func (f diskFiles) Save(ctx context.Context, _, cp string) error {
	name := file(hostdisk.AreaSwap, filepath.Base(cp))
	if _, err := f.m.op(ctx, "checkpoint", "yes", dbMain, none); err != nil {
		return err
	}
	c, err := f.m.op(ctx, "copy", "yes", dbMain, name)
	if err != nil {
		return err
	}
	src, err := f.m.op(ctx, "hash", "yes", dbMain, none)
	if err != nil {
		return err
	}
	if !src.Exists || src.SHA256 != c.SHA256 {
		return fmt.Errorf("the pre-update copy does not match the database")
	}
	return nil
}

func (f diskFiles) Restore(ctx context.Context, _, cp string) error {
	name := file(hostdisk.AreaSwap, filepath.Base(cp))
	for _, suf := range []string{"-wal", "-shm"} {
		if _, err := f.m.op(ctx, "rm", "yes", file(hostdisk.AreaDB, paneldb.FileName+suf), none); err != nil {
			return err
		}
	}
	c, err := f.m.op(ctx, "copy", "yes", name, dbMain)
	if err != nil {
		return err
	}
	src, err := f.m.op(ctx, "hash", "", name, none)
	if err != nil {
		return err
	}
	if !src.Exists || src.SHA256 != c.SHA256 {
		return fmt.Errorf("the restored database does not match its copy")
	}
	return nil
}

// Recover finishes an Update an earlier helper left on a moved install: the
// copy its journal names goes back over the database, then the moved journal.
func (f diskFiles) Recover(ctx context.Context, dataDir string) error {
	j, err := paneldb.ReadJournal(dataDir)
	if err != nil {
		return err
	}
	if !j.Moved || j.Direction != paneldb.DirUpdate || j.Phase == paneldb.PhaseNone {
		return nil
	}
	if j.RollbackBlocked() {
		return fmt.Errorf("%w. Not rolling back, the journal stays. Run stackr-install", ErrCopyGone)
	}
	if _, err := os.Stat(j.Copy); err == nil {
		if err := f.Restore(ctx, j.Dst, j.Copy); err != nil {
			return err
		}
	}
	if err := paneldb.WriteJournal(dataDir, j.MovedBase()); err != nil {
		return err
	}
	paneldb.DropCopy(j.Copy)
	return nil
}

// ErrCopyGone is paneldb.ErrCopyMissing for the helper.
var ErrCopyGone = paneldb.ErrCopyMissing
