package backup

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Point-in-time recovery archive layout, keyed by the instance's data volume
// (unique per copy of the data):
//
//	stackr/<org>/<data-volume-id>/pitr/base/<ts>-<slug>.tar.gz.age
//	stackr/<org>/<data-volume-id>/pitr/wal/<first>-<last>.tar.gz.age
//	stackr/<org>/<data-volume-id>/pitr/wal/<file>.history-<file>.history.tar.gz.age
//
// A WAL bundle holds finished segments (and .backup label files) named
// <first>-<last>, 24-hex segment names. Each timeline .history file travels
// in a bundle of its own, so retention can tell it by name and never drop it.

const (
	PITR      = "pitr"
	BaseDir   = "base"
	WALDir    = "wal"
	BundleExt = ".tar.gz.age"
	// segLen is a WAL segment's name: timeline, log, segment, 8 hex each.
	segLen = 24
)

// PITRRoot is where an instance's point-in-time archive lives.
func PITRRoot(orgID, dataVolumeID string) string { return Prefix(orgID, dataVolumeID, PITR) }

// PITREpochRoot is PITRRoot of the archive's epoch: the first epoch is the
// root itself, a later one a sibling under its own volume key, so that no
// epoch's prefix lies under another's.
func PITREpochRoot(orgID, dataVolumeID string, epoch int) string {
	if epoch > 0 {
		dataVolumeID += "-e" + strconv.Itoa(epoch)
	}
	return PITRRoot(orgID, dataVolumeID)
}

// IsPITRKey says whether an object key is in a point-in-time archive.
func IsPITRKey(key string) bool { return strings.Contains(key, "/"+PITR+"/") }

// WALKey is the object key of the bundle holding segments first through last.
func WALKey(root, first, last string) string {
	return root + "/" + WALDir + "/" + first + "-" + last + BundleExt
}

// Segment is the 24-hex segment name a WAL file name starts with ("" when it
// is not one): a segment, or a .backup label file named after its segment.
func Segment(file string) string {
	if len(file) < segLen || !isHex(file[:segLen]) || (len(file) > segLen && file[segLen] != '.') {
		return ""
	}
	return file[:segLen]
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return s != ""
}

// IsHistory says whether file is a timeline history file (00000002.history).
func IsHistory(file string) bool {
	h, ok := strings.CutSuffix(file, ".history")
	return ok && len(h) == 8 && isHex(h)
}

// WALName is a file the archive dir may hold and the shipper may ship: a
// segment, a .backup label or a .history file. Anything else is left alone,
// and no other name is ever passed to tar or rm.
func WALName(file string) bool {
	if IsHistory(file) {
		return true
	}
	s := Segment(file)
	return s != "" && (file == s || strings.HasSuffix(file, ".backup"))
}

// BundleLast is the last segment a WAL bundle key holds; history is true for
// a .history bundle, which retention never deletes. ok is false for any key
// that is not a bundle.
func BundleLast(key string) (last string, history, ok bool) {
	name, found := strings.CutSuffix(key[strings.LastIndex(key, "/")+1:], BundleExt)
	if !found {
		return "", false, false
	}
	if strings.Contains(name, ".history") {
		return "", true, true
	}
	if len(name) != 2*segLen+1 || name[segLen] != '-' || Segment(name[:segLen]) == "" || Segment(name[segLen+1:]) == "" {
		return "", false, false
	}
	return name[segLen+1:], false, true
}

// Window picks base backups for retention: every base that finished within
// days of now stays, plus the newest older one (the anchor the window's
// oldest moment is replayed from); the rest are dropped. walBefore is the
// segment the oldest kept base started at: a WAL bundle that ends before it
// can never be replayed. ok is false when no base finished well (no good
// base, no pruning). bases is any mix of runs; only finished ones with a
// recorded wal_start count. The anchor is the newest older base on lin's
// lineage, as restore reads it: an older base off the lineage is dropped, and
// never takes the anchor's place.
func Window(bases []store.BackupRun, lin Lineage, now time.Time, days int) (drop []store.BackupRun, walBefore string, ok bool) {
	var good []store.BackupRun
	for _, r := range bases {
		if r.Status == Done && r.ObjectKey != "" && r.FinishedAt != nil && r.WalStart != nil && *r.WalStart != "" {
			good = append(good, r)
		}
	}
	if len(good) == 0 {
		return nil, "", false
	}
	slices.SortFunc(good, func(a, b store.BackupRun) int { return b.FinishedAt.Compare(*a.FinishedAt) })
	cutoff := now.AddDate(0, 0, -days)
	anchored := false
	for _, r := range good {
		if r.FinishedAt.After(cutoff) {
			walBefore = minSeg(walBefore, *r.WalStart)
			continue
		}
		if !anchored && lin.allows(baseStop(r)) {
			anchored = true
			walBefore = minSeg(walBefore, *r.WalStart)
			continue
		}
		drop = append(drop, r)
	}
	return drop, walBefore, true
}

func minSeg(a, b string) string {
	if a == "" || b < a {
		return b
	}
	return a
}

// StaleWAL are the bundle keys safe to delete: not .history, ending strictly
// before the segment walBefore. A key that is not a bundle is never listed.
func StaleWAL(keys []string, walBefore string) []string {
	var out []string
	for _, k := range keys {
		if last, hist, ok := BundleLast(k); ok && !hist && last < walBefore[:min(len(walBefore), segLen)] {
			out = append(out, k)
		}
	}
	return out
}

// SetWalStart records the WAL segment a base backup started at.
func (l *Leaf) SetWalStart(ctx context.Context, r store.BackupRun, seg string) (store.BackupRun, error) {
	r.WalStart = &seg
	return r, l.runs.Update(ctx, r)
}

// Owns says whether any run recorded an object under root on destination
// destID: the guard that stops a copy of an instance writing into another's
// archive. It looks at every run with a key, failed base runs included (they
// keep the key they tried to write). A run on another destination proves
// nothing about this one.
func (l *Leaf) Owns(ctx context.Context, root, destID string) (bool, error) {
	rs, err := l.runs.ListByPrefix(ctx, root+"/")
	return len(onDest(rs, destID)) > 0, err
}

func onDest(rs []store.BackupRun, destID string) []store.BackupRun {
	return slices.DeleteFunc(rs, func(r store.BackupRun) bool { return r.DestID != destID })
}

// Bases are the finished and failed base runs of a data volume's archive on
// destination destID, newest first. A run that started before since is left
// out (zero = none are): PITR was off, or archiving not running yet, then.
func (l *Leaf) Bases(ctx context.Context, root, destID string, since time.Time) ([]store.BackupRun, error) {
	rs, err := l.runs.ListByPrefix(ctx, root+"/"+BaseDir+"/")
	rs = slices.DeleteFunc(onDest(rs, destID), func(r store.BackupRun) bool { return r.CreatedAt.Before(since) })
	slices.SortFunc(rs, func(a, b store.BackupRun) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return rs, err
}

// DropRun deletes a run row (its object is already gone).
func (l *Leaf) DropRun(ctx context.Context, id string) error { return l.runs.Delete(ctx, id) }
