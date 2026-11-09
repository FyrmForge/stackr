package operated

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPGReleaseTable(t *testing.T) {
	if len(PGReleases) != 5 || PGReleases[0].Major != 14 || PGNewestMajor() != 18 {
		t.Fatalf("table = %+v", PGReleases)
	}
	for i, r := range PGReleases {
		if i > 0 && r.Major != PGReleases[i-1].Major+1 {
			t.Errorf("majors are not consecutive at %d", r.Major)
		}
		if got := ImageMajor(r.Image()); got != r.Major {
			t.Errorf("%s names major %d, not %d", r.Image(), got, r.Major)
		}
		if !strings.HasPrefix(r.Tag(), strconv.Itoa(r.Major)+".") {
			t.Errorf("tag %s", r.Tag())
		}
	}
}

func TestPGEOLFlipsByDate(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) }
	for _, c := range []struct {
		major int
		now   time.Time
		eol   bool
	}{
		{14, day(2026, 11, 11), false},
		{14, day(2026, 11, 12), true}, // the final release day
		{14, day(2027, 1, 1), true},
		{15, day(2027, 1, 1), false},
		{15, day(2027, 11, 12), true},
		{18, day(2029, 1, 1), false},
		{13, day(2020, 1, 1), true}, // older than the table
		{19, day(2020, 1, 1), false},
	} {
		if got := PGEOL(c.major, c.now); got != c.eol {
			t.Errorf("%d at %s: eol = %v", c.major, c.now.Format(time.DateOnly), got)
		}
	}
	if got := PGPickable(day(2026, 11, 11)); !slices.Equal(got, []int{14, 15, 16, 17, 18}) {
		t.Errorf("before: %v", got)
	}
	if got := PGPickable(day(2026, 11, 13)); !slices.Equal(got, []int{15, 16, 17, 18}) {
		t.Errorf("after: %v", got)
	}
}

func TestPGPick(t *testing.T) {
	before := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	after := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		major   int
		image   string
		now     time.Time
		want    string
		wantMaj int
		bad     bool
	}{
		{0, "", before, PGReleases[4].Image(), 18, false}, // newest by default
		{14, "", before, PGReleases[0].Image(), 14, false},
		{16, "", after, PGReleases[2].Image(), 16, false},
		{14, "", after, "", 0, true},  // over by date
		{13, "", before, "", 0, true}, // not run
		{19, "", before, "", 0, true}, // not run yet
		{0, "postgres:16.2", before, "postgres:16.2", 16, false},
		{16, "postgres:16.2", before, "postgres:16.2", 16, false},
		{17, "postgres:16.2", before, "", 0, true},  // image and major disagree
		{0, "postgres:14.1", after, "", 0, true},    // an image gets no way round EOL
		{0, "postgres:latest", before, "", 0, true}, // names no major
	} {
		img, maj, err := PGPick(c.major, c.image, c.now)
		if (err != nil) != c.bad || img != c.want || maj != c.wantMaj {
			t.Errorf("pick(%d, %q, %s) = %q %d %v", c.major, c.image, c.now.Format(time.DateOnly), img, maj, err)
		}
	}
}

func TestPGUpdate(t *testing.T) {
	cur := PGReleases[3] // 17
	for _, c := range []struct {
		image string
		to    string
		ok    bool
	}{
		{"postgres:17.1", cur.Image(), true},
		{"postgres:17", cur.Image(), true}, // floating
		{cur.Image(), "", false},
		{"postgres:17.99", "", false}, // newer than the table: never a downgrade
		{"postgres:17-alpine", "", false},
		{"postgres:17.1-alpine", "", false},
		{"postgres:13.1", "", false},
		{"postgres:latest", "", false},
		{"registry.example.com/postgres:17.1", "", false},
		{"postgres@sha256:abc", "", false},
		{"library/postgres:17.1", cur.Image(), true},
	} {
		if to, ok := PGUpdate(c.image); to != c.to || ok != c.ok {
			t.Errorf("update(%s) = %q %v", c.image, to, ok)
		}
	}
}

// 14, 15 and 16 take no allow_alter_system (it arrived in 17); every major
// gets the archive flags when PITR is on.
func TestPGFlagsEveryTableMajor(t *testing.T) {
	for _, r := range PGReleases {
		f := strings.Join(pgFlags(r.Major, 7, 0), " ")
		if has := strings.Contains(f, "allow_alter_system"); has != (r.Major >= 17) {
			t.Errorf("%d: allow_alter_system = %v", r.Major, has)
		}
		for _, want := range []string{"shared_preload_libraries=pg_stat_statements", "wal_log_hints=on", "archive_mode=on"} {
			if !strings.Contains(f, want) {
				t.Errorf("%d: no %s in %s", r.Major, want, f)
			}
		}
	}
}

// With every major past its end of life a default pick is a refusal, not a panic.
func TestPGPickAllEOL(t *testing.T) {
	if _, _, err := PGPick(0, "", time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Error("picked a major past its end of life")
	}
}
