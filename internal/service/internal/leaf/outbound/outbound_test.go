package outbound_test

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/outbound"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type fake struct {
	calls [][]store.TileOutbound
	fail  []string
}

func (f *fake) AddAll(_ context.Context, r []store.TileOutbound) ([]string, error) {
	f.calls = append(f.calls, r)
	return f.fail, nil
}
func (f *fake) ListByEnv(context.Context, string) ([]store.TileOutbound, error) { return nil, nil }

// Ticks sum in memory; one write per minute carries the total.
func TestFlushDue(t *testing.T) {
	ctx, f, t0 := context.Background(), &fake{}, time.Unix(1000, 0)
	l := outbound.New(f)
	for i := 0; i < 12; i++ {
		at := t0.Add(time.Duration(i) * 5 * time.Second)
		l.Add("a", 10, at)
		if err := l.FlushDue(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.calls) != 1 || f.calls[0][0].Bytes != 10 {
		// first tick is already a minute past the zero time
		t.Fatalf("calls = %+v", f.calls)
	}
	at := t0.Add(2 * time.Minute)
	l.Add("a", 5, at)
	_ = l.FlushDue(ctx, at)
	if len(f.calls) != 2 || f.calls[1][0].Bytes != 115 {
		t.Fatalf("calls = %+v", f.calls)
	}
}

// A row the store failed to write keeps its sum for the next flush; the
// persisted ones are cleared.
func TestFlushKeepsFailedRows(t *testing.T) {
	ctx, f, t0 := context.Background(), &fake{fail: []string{"bad"}}, time.Unix(1000, 0)
	l := outbound.New(f)
	l.Add("bad", 7, t0)
	l.Add("good", 3, t0)
	if err := l.FlushDue(ctx, t0); err != nil {
		t.Fatal(err)
	}
	f.fail = nil
	_ = l.FlushDue(ctx, t0.Add(2*time.Minute))
	last := f.calls[len(f.calls)-1]
	if len(last) != 1 || last[0].TileID != "bad" || last[0].Bytes != 7 {
		t.Fatalf("second flush = %+v, want only bad with 7", last)
	}
}
