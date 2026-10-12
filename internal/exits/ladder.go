package exits

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Rung is one exit in a ladder, ready to route over.
type Rung struct {
	NodeID string
	// Own marks the node's own uplink: routing it means removing the subject's
	// rules, nothing is installed.
	Own bool
	// Iface is the exit link (empty for the own uplink) and Table its routing
	// table.
	Iface string
	Table int
	// Target is what the pinger is asked to reach through Iface.
	Target string
}

// Pinger probes one rung: a reply to Target sent out Iface (a plain ping for
// the own uplink) returns nil.
type Pinger interface {
	Ping(ctx context.Context, r Rung) error
}

// PingerFunc adapts a function to Pinger.
type PingerFunc func(ctx context.Context, r Rung) error

func (f PingerFunc) Ping(ctx context.Context, r Rung) error { return f(ctx, r) }

// State is what the ladder reports.
type State struct {
	// Active is the index of the rung in use, -1 when none (Down or not yet
	// probed).
	Active int
	NodeID string
	// Down is true when every rung failed: the subject is blackholed. Reason
	// then says why, rung by rung.
	Down   bool
	Reason string
	Since  time.Time
}

// Ladder walks one subject down its rungs when the live one stops answering
// and back up when a higher one answers again. One Tick is one health pass;
// Run ticks every interval. Safe for concurrent State calls.
type Ladder struct {
	Subject Subject
	Rungs   []Rung
	Cmd     Runner
	Probe   Pinger
	// FailAfter consecutive failed probes before the live rung is given up
	// (default 3). RecoverAfter consecutive good probes before a better rung
	// is taken back (default 2). A rung below the live one is taken on one
	// good probe: something is better than nothing.
	FailAfter    int
	RecoverAfter int
	// Now is the clock, time.Now when nil. OnChange is called after the rules
	// of a new state are applied.
	Now      func() time.Time
	OnChange func(State)

	mu   sync.Mutex
	st   State
	init bool
	good []int
	bad  []int
	errs []string
}

// NewLadder is a ladder with nothing applied yet; the first Tick does that.
func NewLadder(s Subject, rungs []Rung, r Runner, p Pinger) *Ladder {
	return &Ladder{Subject: s, Rungs: rungs, Cmd: r, Probe: p, st: State{Active: -1}}
}

// State is the last reported state.
func (l *Ladder) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st
}

func (l *Ladder) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func orDefault(v, d int) int {
	if v < 1 {
		return d
	}
	return v
}

// probe pings rung i and updates its streaks.
func (l *Ladder) probe(ctx context.Context, i int) bool {
	err := l.Probe.Ping(ctx, l.Rungs[i])
	if err == nil {
		l.good[i]++
		l.bad[i] = 0
		l.errs[i] = ""
		return true
	}
	l.bad[i]++
	l.good[i] = 0
	l.errs[i] = err.Error()
	return false
}

// Tick runs one health pass, switches rung when needed and returns the state.
// A rule failure is returned and leaves the old state in place, so the next
// tick tries again.
func (l *Ladder) Tick(ctx context.Context) (State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.good) != len(l.Rungs) {
		l.good, l.bad, l.errs = make([]int, len(l.Rungs)), make([]int, len(l.Rungs)), make([]string, len(l.Rungs))
	}
	failAfter, recoverAfter := orDefault(l.FailAfter, 3), orDefault(l.RecoverAfter, 2)

	next := l.st.Active
	active := l.st.Active
	switch {
	case active < 0:
		// Down or fresh: the first rung that answers once.
		for i := range l.Rungs {
			if l.probe(ctx, i) {
				next = i
				break
			}
		}
	default:
		// Upward first, so a recovered better rung wins over a healthy one.
		for i := 0; i < active; i++ {
			if l.probe(ctx, i) && l.good[i] >= recoverAfter {
				next = i
				break
			}
		}
		if next == active && !l.probe(ctx, active) && l.bad[active] >= failAfter {
			next = -1
			for i := active + 1; i < len(l.Rungs); i++ {
				if l.probe(ctx, i) {
					next = i
					break
				}
			}
			if next < 0 {
				// A rung above that answered once is as good as one below.
				for i := 0; i < active; i++ {
					if l.good[i] > 0 {
						next = i
						break
					}
				}
			}
		}
	}

	if l.init && next == l.st.Active && (next >= 0 || l.st.Down) {
		return l.st, nil
	}
	if err := l.apply(ctx, next); err != nil {
		return l.st, err
	}
	st := State{Active: next, Since: l.now()}
	if next >= 0 {
		st.NodeID = l.Rungs[next].NodeID
	} else {
		st.Down, st.Reason = true, l.reason()
	}
	l.st, l.init = st, true
	if l.OnChange != nil {
		l.OnChange(st)
	}
	return st, nil
}

func (l *Ladder) reason() string {
	if len(l.Rungs) == 0 {
		return "no exits listed"
	}
	var p []string
	for i, r := range l.Rungs {
		p = append(p, fmt.Sprintf("%s: %s", r.NodeID, l.errs[i]))
	}
	return fmt.Sprintf("no exit answers (%s)", strings.Join(p, "; "))
}

func (l *Ladder) apply(ctx context.Context, i int) error {
	var steps []Step
	var err error
	switch {
	case i < 0:
		steps, err = l.Subject.Blackhole()
	case l.Rungs[i].Own:
		steps = l.Subject.Clear()
	default:
		steps, err = l.Subject.Route(l.Rungs[i].Table)
	}
	if err != nil {
		return err
	}
	return Apply(ctx, l.Cmd, steps)
}

// Loop ticks every interval (default 5s) until ctx ends. Errors go to onErr.
func (l *Ladder) Loop(ctx context.Context, every time.Duration, onErr func(error)) {
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := l.Tick(ctx); err != nil && onErr != nil {
			onErr(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
