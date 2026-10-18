package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The skew is the worker's clock against the midpoint of the request, so a
// slow round trip does not count as skew; an older worker reports none.
func TestClockSkew(t *testing.T) {
	sent := time.Unix(1000, 0)
	for _, c := range []struct {
		rtt, ahead time.Duration
	}{
		{0, 0},
		{4 * time.Second, 0},
		{4 * time.Second, 2 * time.Minute},
		{100 * time.Millisecond, -45 * time.Second},
	} {
		mid := sent.Add(c.rtt / 2)
		got, ok := clockSkew(sent, sent.Add(c.rtt), mid.Add(c.ahead).UnixMilli())
		if !ok || got != c.ahead {
			t.Errorf("rtt %v ahead %v: got %v %v", c.rtt, c.ahead, got, ok)
		}
	}
	if _, ok := clockSkew(sent, sent, 0); ok {
		t.Error("an unreported time is known")
	}
}

// clock.skew fires once per crossing of 30 s either way and resets when the
// clock is back; an unknown skew changes nothing.
func TestClockAlertOncePerCrossing(t *testing.T) {
	ctx := context.Background()
	o := newWorld(t).orch
	h := newHook(t)
	alertsTo(t, o, h, "")
	n := store.Node{ID: "n1", Name: "n1"}
	for i, s := range []struct {
		skew  time.Duration
		known bool
		fires int
	}{
		{time.Second, true, 0},
		{2 * time.Minute, true, 1},
		{3 * time.Minute, true, 0},
		{0, false, 0}, // an older worker: still set
		{2 * time.Minute, true, 0},
		{time.Second, true, 0}, // back
		{-31 * time.Second, true, 1},
	} {
		o.clockAlert(ctx, n, s.skew, s.known)
		if got := h.events(t, o); len(got) != s.fires || (s.fires == 1 && got[0] != AlertClockSkew+":"+SevWarning) {
			t.Errorf("step %d: %v", i, got)
		}
	}
}

// Each kind of warning on its own, and none for a healthy node.
func TestWarningsOf(t *testing.T) {
	const g = 1 << 30
	for _, c := range []struct {
		in     warnIn
		kind   string
		label  string
		detail string
	}{
		{warnIn{Behind: true, Version: "0.8.1", Panel: "0.8.2"}, WarnVersion, "behind", "Runs 0.8.1, the panel runs 0.8.2."},
		{warnIn{Behind: true, TooOld: true}, WarnVersion, "too old", "Runs an older build"},
		{warnIn{CertExpired: true}, WarnCert, "Cert expired, rejoin needed", "Rejoin it."},
		{warnIn{CertDays: 1}, WarnCert, "Cert expires in 1 day", "expires in 1 day."},
		{warnIn{Skew: 2 * time.Minute, SkewOK: true}, WarnClock, "Clock 2m ahead", "2m ahead of the panel"},
		{warnIn{Skew: -45 * time.Second, SkewOK: true}, WarnClock, "Clock 45s behind", "45s behind"},
		{warnIn{DiskFree: 5 * g, DiskTotal: 100 * g}, WarnDisk, "Disk low", "5.0 GB (5%) free"},
	} {
		ws := warningsOf(c.in)
		if len(ws) != 1 || ws[0].Kind != c.kind || ws[0].Label != c.label || !strings.Contains(ws[0].Detail, c.detail) {
			t.Errorf("%+v: %+v", c.in, ws)
		}
	}
	for _, in := range []warnIn{
		{},
		{Skew: 29 * time.Second, SkewOK: true},
		{Skew: time.Hour}, // not known
		{DiskFree: 50 * g, DiskTotal: 100 * g},
	} {
		if ws := warningsOf(in); len(ws) != 0 {
			t.Errorf("%+v warns: %+v", in, ws)
		}
	}
}
