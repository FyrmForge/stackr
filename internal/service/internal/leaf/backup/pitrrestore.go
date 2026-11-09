package backup

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Point-in-time restore: which base and which WAL bundles a restore to a
// time reads, and the rules about the time itself. Pure; the flow fetches.

// BundleFirst is the first segment a WAL bundle key holds, as BundleLast is
// the last.
func BundleFirst(key string) (first string, history, ok bool) {
	name, found := strings.CutSuffix(key[strings.LastIndex(key, "/")+1:], BundleExt)
	if !found {
		return "", false, false
	}
	if strings.Contains(name, ".history") {
		return "", true, true
	}
	if _, _, ok := BundleLast(key); !ok {
		return "", false, false
	}
	return name[:segLen], false, true
}

// BaseWalStart is the segment a base backup's key names as its start
// (<ts>-<walstart>-<slug>.tar.gz.age), "" when it names none: the archive
// stays readable when its run rows are lost.
func BaseWalStart(key string) string {
	name := key[strings.LastIndex(key, "/")+1:]
	const tsLen = len("20060102-150405")
	if len(name) < tsLen+1+segLen+1 || name[tsLen] != '-' || name[tsLen+1+segLen] != '-' {
		return ""
	}
	return Segment(name[tsLen+1 : tsLen+1+segLen])
}

// Bundle is one WAL bundle object: the segments First through Last, or a
// timeline history file.
type Bundle struct {
	Key         string
	First, Last string
	History     bool
}

// Bundles are the WAL bundles among keys, history first, then by key.
func Bundles(keys []string) []Bundle {
	var out []Bundle
	for _, k := range keys {
		last, hist, ok := BundleLast(k)
		if !ok {
			continue
		}
		b := Bundle{Key: k, History: hist}
		if !hist {
			b.First, _, _ = BundleFirst(k)
			b.Last = last
		}
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b Bundle) int {
		if a.History != b.History {
			if a.History {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out
}

func timeline(seg string) string { return seg[:min(8, len(seg))] }

// segIndex is a segment's position in its timeline: 256 segments per log
// file (16 MiB segments).
func segIndex(seg string) int64 {
	hi, err1 := strconv.ParseInt(seg[8:16], 16, 64)
	lo, err2 := strconv.ParseInt(seg[16:24], 16, 64)
	if err1 != nil || err2 != nil {
		return 0
	}
	return hi*256 + lo
}

// SegmentBytes is a WAL segment's size. ponytail: initdb's default; an
// instance initialised with another --wal-segsize is sized wrong here, and
// only the disk check reads this.
const SegmentBytes = 16 << 20

// Plan is what a restore to a point in time reads: one base, and the WAL
// bundles from where it started.
type Plan struct {
	Base     store.BackupRun
	WalStart string   // where the base's replay starts
	Bundles  []Bundle // history bundles, then segments from WalStart on
	Segments int      // segments the bundles hold, for the disk check
}

// baseStop is the segment a base needs shipped, from the run row.
func baseStop(r store.BackupRun) string {
	if r.WalStop == nil {
		return ""
	}
	return *r.WalStop
}

// baseStart is the base's start segment: the run row's, else the one its
// key names.
func baseStart(r store.BackupRun) string {
	if r.WalStart != nil && *r.WalStart != "" {
		return *r.WalStart
	}
	return BaseWalStart(r.ObjectKey)
}

// Lineage is the timeline history a restore follows to "latest": Tip is the
// newest timeline with a history file ("" = none, everything is on the first),
// Forks maps each ancestor timeline to the index of the segment it ended in.
type Lineage struct {
	Tip   string
	Forks map[string]int64
}

// ParseLineage reads the tip timeline's history file: one line per ancestor,
// "<parent timeline>\t<switch LSN>\t<reason>". A line it cannot read is skipped.
func ParseLineage(tip string, history string) Lineage {
	l := Lineage{Tip: tip, Forks: map[string]int64{}}
	for _, line := range strings.Split(history, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		parent, err := strconv.ParseUint(strings.TrimSpace(f[0]), 10, 32)
		hi, lo, ok := strings.Cut(strings.TrimSpace(f[1]), "/")
		h, err2 := strconv.ParseUint(hi, 16, 32)
		w, err3 := strconv.ParseUint(lo, 16, 32)
		if err != nil || !ok || err2 != nil || err3 != nil {
			continue
		}
		l.Forks[fmt.Sprintf("%08X", parent)] = int64(h)*256 + int64(w)/SegmentBytes
	}
	return l
}

// allows says whether a base that ends in segment stop lies on the path to
// the tip: on the tip itself, or on an ancestor before it forked. A base on
// an older timeline past the fork makes postgres refuse the restore
// ("requested timeline is not a child of this server's history").
func (l Lineage) allows(stop string) bool {
	tl := timeline(stop)
	if l.Tip == "" || tl == l.Tip {
		return true
	}
	fork, ok := l.Forks[tl]
	return ok && segIndex(stop) < fork
}

// usable are the bases that finished well, have a start and an end, and
// whose shipped WAL holds the end: a bundle whose range contains the end
// segment (a bundle shipped across a promote spans two timelines, and the
// names compare timeline first), on the tip's lineage. A base whose last
// segment is not archived yet restores nothing. Newest first.
func usable(bases []store.BackupRun, bundles []Bundle, lin Lineage) []store.BackupRun {
	var out []store.BackupRun
	for _, r := range bases {
		stop, start := baseStop(r), baseStart(r)
		if r.Status != Done || r.ObjectKey == "" || r.FinishedAt == nil || Segment(stop) == "" || Segment(start) == "" {
			continue
		}
		if slices.ContainsFunc(bundles, func(b Bundle) bool {
			return !b.History && b.First <= stop && stop <= b.Last
		}) && lin.allows(stop) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b store.BackupRun) int { return b.FinishedAt.Compare(*a.FinishedAt) })
	return out
}

// Restorable is the oldest finish time among usable bases; ok is false when
// none is usable. The newest moment is the shipper's (the archive is only
// known current up to its last clean tick), so the caller owns it.
func Restorable(bases []store.BackupRun, keys []string, lin Lineage) (from time.Time, ok bool) {
	us := usable(bases, Bundles(keys), lin)
	if len(us) == 0 {
		return time.Time{}, false
	}
	return *us[len(us)-1].FinishedAt, true
}

// Range is a stretch of time a restore can reach.
type Range struct{ From, To time.Time }

// reach is how far a restore from us[i] (usable bases, newest first) gets.
// hole is the first segment missing after its start ("" = none: it reaches
// the newest WAL). With a hole, end is the last moment before it: the newest
// of marks (when each failover left the old timeline's archive known
// current) that is after the base and before the next base the hole does not
// bind; resume is that base's finish, where restores work again. ok is false
// when no mark explains the hole: the base reaches nowhere.
func reach(us []store.BackupRun, i int, bundles []Bundle, lin Lineage, marks []time.Time) (hole string, end, resume time.Time, ok bool) {
	hole = firstHole(bundles, baseStart(us[i]), lin)
	if hole == "" {
		return hole, end, resume, true
	}
	for j := i - 1; j >= 0; j-- {
		if firstHole(bundles, baseStart(us[j]), lin) != hole {
			resume = *us[j].FinishedAt
			break
		}
	}
	for _, m := range marks {
		if !m.Before(*us[i].FinishedAt) && (resume.IsZero() || m.Before(resume)) {
			end, ok = m, true
		}
	}
	return hole, end, resume, ok
}

// Ranges are the stretches a restore can reach, oldest first: each usable
// base reaches from its finish to the newest WAL (to), or, when the WAL has a
// hole after it (an unplanned promote lost segments), to the last moment
// before the hole; stretches that meet are one.
func Ranges(bases []store.BackupRun, keys []string, lin Lineage, marks []time.Time, to time.Time) []Range {
	bundles := Bundles(keys)
	us := usable(bases, bundles, lin)
	var out []Range
	for i := len(us) - 1; i >= 0; i-- {
		from, end := *us[i].FinishedAt, to
		hole, e, _, ok := reach(us, i, bundles, lin, marks)
		if hole != "" {
			end = e
		}
		if !ok || end.Before(from) {
			continue
		}
		if n := len(out); n > 0 && !from.After(out[n-1].To) {
			out[n-1].To = later(out[n-1].To, end)
		} else {
			out = append(out, Range{from, end})
		}
	}
	return out
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// PickBase picks what a restore to at reads: the newest usable base that
// finished at or before at (the newest of all for latest), and the bundles
// from its start on. A hole in the WAL refuses the plan only when the restore
// would reach it: a target before the hole (marks say where it begins) stops
// ahead of it, a target inside the gap is refused by name.
func PickBase(bases []store.BackupRun, keys []string, lin Lineage, marks []time.Time, at time.Time, latest bool) (Plan, error) {
	bundles := Bundles(keys)
	us := usable(bases, bundles, lin)
	if len(us) == 0 {
		return Plan{}, errs.Conflictf("no base backup can be restored yet: one can, once it has finished and its WAL has been shipped")
	}
	i := 0
	if !latest {
		i = slices.IndexFunc(us, func(r store.BackupRun) bool { return !r.FinishedAt.After(at) })
		if i < 0 {
			return Plan{}, errs.Invalidf("at", "%s is before the oldest base backup that can be restored (%s)",
				at.UTC().Format(time.RFC3339), us[len(us)-1].FinishedAt.UTC().Format(time.RFC3339))
		}
	}
	p := Plan{Base: us[i], WalStart: baseStart(us[i])}
	for _, b := range bundles {
		if b.History || b.Last >= p.WalStart {
			p.Bundles = append(p.Bundles, b)
		}
		if !b.History && b.Last >= p.WalStart {
			p.Segments += int(segIndex(b.Last)-segIndex(b.First)) + 1
		}
	}
	if hole, end, resume, ok := reach(us, i, bundles, lin, marks); hole != "" && !(ok && !latest && !at.After(end)) {
		if ok && !latest && !resume.IsZero() {
			return Plan{}, errs.Conflictf("%s is in a gap in the WAL archive (segments were lost when the primary failed over): restore to a time up to %s, or from %s on",
				at.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), resume.UTC().Format(time.RFC3339))
		}
		return Plan{}, errs.Conflictf("the WAL archive is missing segment %s, between this base and the newest shipped WAL; a restore would stop early", hole)
	}
	return p, nil
}

// firstHole is the first segment missing between start and the newest bundle
// on the lineage, "" for none. Timelines are followed along the lineage: an
// ancestor is needed up to the segment its child forks in, which the child's
// file holds. A bundle shipped across a promote spans two timelines; its ends
// are taken as open.
func firstHole(bundles []Bundle, start string, lin Lineage) string {
	type span struct{ lo, hi int64 }
	byTL := map[string][]span{}
	for _, b := range bundles {
		if b.History {
			continue
		}
		f, l := timeline(b.First), timeline(b.Last)
		if f == l {
			byTL[f] = append(byTL[f], span{segIndex(b.First), segIndex(b.Last)})
			continue
		}
		byTL[f] = append(byTL[f], span{segIndex(b.First), math.MaxInt64})
		byTL[l] = append(byTL[l], span{0, segIndex(b.Last)})
	}
	chain := slices.Sorted(maps.Keys(lin.Forks))
	if lin.Tip != "" && !slices.Contains(chain, lin.Tip) {
		chain = append(chain, lin.Tip)
	}
	tl0 := timeline(start)
	i0 := slices.Index(chain, tl0)
	if i0 < 0 {
		i0, chain = 0, []string{tl0}
	}
	chain = chain[i0:]
	last := -1 // the newest timeline on the lineage that has WAL
	for i, tl := range chain {
		if len(byTL[tl]) > 0 {
			last = i
		}
	}
	idx := segIndex(start)
	for i := 0; i <= last; i++ {
		tl := chain[i]
		hi := lin.Forks[tl] - 1
		if i == last {
			hi = 0
			for _, sp := range byTL[tl] {
				hi = max(hi, sp.lo)
				if sp.hi != math.MaxInt64 {
					hi = max(hi, sp.hi)
				}
			}
		}
		for idx <= hi {
			next := int64(-1)
			for _, sp := range byTL[tl] {
				if sp.lo <= idx && idx <= sp.hi {
					next = max(next, sp.hi)
				}
			}
			if next < 0 {
				return fmt.Sprintf("%s%08X%08X", tl, idx/256, idx%256)
			}
			if next == math.MaxInt64 {
				break
			}
			idx = next + 1
		}
		idx = lin.Forks[tl] // the child's file starts at the fork segment
	}
	return ""
}

// ParseAt reads a restore time: RFC3339 with an offset (or Z), or "latest".
func ParseAt(s string) (at time.Time, latest bool, err error) {
	if s == "latest" {
		return time.Time{}, true, nil
	}
	at, err = time.Parse(time.RFC3339, s)
	if err != nil {
		return at, false, errs.Invalidf("at", "%q is not a time; use RFC3339 with an offset (2026-11-07T10:42:00+01:00, or Z) or latest", s)
	}
	return at.UTC(), false, nil
}

// BranchMargin is how close to now a branch --at may be: the archive trails
// the database by about this much, and a plain branch copies it as it is.
const BranchMargin = 2 * time.Minute

// CheckAt refuses a restore time outside [from, to], naming both bounds, and
// for a branch one within BranchMargin of now.
func CheckAt(at time.Time, latest bool, from, to, now time.Time, branch bool) error {
	switch {
	case latest:
		return nil
	case at.Before(from) || at.After(to):
		return errs.Invalidf("at", "%s is outside what can be restored: from %s to %s",
			at.UTC().Format(time.RFC3339), from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	case branch && now.Sub(at) < BranchMargin:
		return errs.Invalidf("at", "%s is within %s of now; branch without --at to copy it as it is",
			at.UTC().Format(time.RFC3339), BranchMargin)
	}
	return nil
}

// DiskNeed is the free space a restore wants on the node: 1.2 x (the base as
// it unpacks + the WAL it replays).
func DiskNeed(baseBytes int64, segments int) int64 {
	return (baseBytes + int64(segments)*SegmentBytes) * 12 / 10
}
