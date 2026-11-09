package operated

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// PGRelease is one supported postgres major: the minor stackr pins new
// instances to, and the day the postgres project stops fixing it.
type PGRelease struct {
	Major int
	Minor int       // the current minor of the official image, bumped by a stackr release
	EOL   time.Time // postgresql.org/support/versioning, the final release day
}

// PGReleases is the one table of what stackr runs, oldest major first. A new
// instance gets postgres:<major>.<minor>; an existing one keeps its tag until
// its owner applies the update.
var PGReleases = []PGRelease{
	{14, 24, pgDay(2026, time.November, 12)},
	{15, 19, pgDay(2027, time.November, 11)},
	{16, 15, pgDay(2028, time.November, 9)},
	{17, 11, pgDay(2029, time.November, 8)},
	{18, 6, pgDay(2030, time.November, 14)},
}

func pgDay(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func (r PGRelease) Tag() string { return fmt.Sprintf("%d.%d", r.Major, r.Minor) }

// Image is the image new instances of the major run.
func (r PGRelease) Image() string { return "postgres:" + r.Tag() }

// PGReleaseOf looks a major up in the table.
func PGReleaseOf(major int) (PGRelease, bool) {
	for _, r := range PGReleases {
		if r.Major == major {
			return r, true
		}
	}
	return PGRelease{}, false
}

// PGEOL says whether the major no longer gets security fixes at now. A major
// older than the table is over; one newer is not.
func PGEOL(major int, now time.Time) bool {
	if r, ok := PGReleaseOf(major); ok {
		return !now.Before(r.EOL)
	}
	return major < PGReleases[0].Major
}

// PGNewestMajor is the newest supported major, the create default.
func PGNewestMajor() int { return PGReleases[len(PGReleases)-1].Major }

// PGPickable are the majors a new instance may pick at now, newest last.
func PGPickable(now time.Time) []int {
	var out []int
	for _, r := range PGReleases {
		if !PGEOL(r.Major, now) {
			out = append(out, r.Major)
		}
	}
	return out
}

// PGPick is the image and major of a new postgres instance. image is what
// the caller named ("" = none) and wins; major 0 is the newest pickable.
// Either must name a major that is supported and not over at now.
func PGPick(major int, image string, now time.Time) (string, int, error) {
	if image != "" {
		got := ImageMajor(image)
		if got == 0 {
			return "", 0, errs.Invalidf("image_ref", "%s names no postgres major; pin a numeric tag such as postgres:17.", image)
		}
		if major != 0 && major != got {
			return "", 0, errs.Invalidf("major", "%s is postgres %d, not %d.", image, got, major)
		}
		major = got
	} else if major == 0 {
		p := PGPickable(now)
		if len(p) == 0 {
			return "", 0, errs.Conflictf("every postgres major stackr runs is past its end of life; update stackr")
		}
		major = p[len(p)-1]
	}
	r, ok := PGReleaseOf(major)
	switch {
	case !ok:
		return "", 0, errs.Invalidf("major", "postgres %d is not one stackr runs; pick %d to %d.", major, PGReleases[0].Major, PGNewestMajor())
	case PGEOL(major, now):
		return "", 0, errs.Invalidf("major", "postgres %d no longer gets security fixes; pick a newer major.", major)
	}
	if image == "" {
		image = r.Image()
	}
	return image, major, nil
}

// PGUpdate is the image the instance would move to on a minor update, when
// its tag is an official postgres:<major> or :<major>.<minor> older than the
// table's. A custom repo or tag is the owner's own and is left alone.
func PGUpdate(image string) (string, bool) {
	repo, tag, ok := strings.Cut(image, ":")
	if !ok || (repo != "postgres" && repo != "library/postgres" && repo != "docker.io/library/postgres") {
		return "", false
	}
	major, minor, _ := strings.Cut(tag, ".")
	mj, err := strconv.Atoi(major)
	if err != nil {
		return "", false
	}
	r, ok := PGReleaseOf(mj)
	if !ok {
		return "", false
	}
	if minor == "" { // the floating tag
		return r.Image(), true
	}
	n, err := strconv.Atoi(minor)
	if err != nil || n >= r.Minor {
		return "", false
	}
	return r.Image(), true
}
