package paneldb

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ProbeName is the file the runtime ticker writes in db/ (not the database).
const ProbeName = ".stackr-probe"

// Ticker settings: one tiny write and fsync every TickEvery, exit after
// TickMax consecutive failures. A panel that exits lands in the boot check,
// and Docker's restart loop keeps trying until the disk is back.
const (
	TickEvery = 10 * time.Second
	TickMax   = 3
	// TickSilentMax is the budget for a probe that gives no answer at all: a
	// slow disk is not a dead one, so it gets the worker's 9 ticks (90s).
	TickSilentMax = 9
)

// ProbeWrite does one tiny write and fsync of the probe file in dir.
func ProbeWrite(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, ProbeName), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(time.Now().UTC().Format(Stamp))
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// Countable: a failure that says the disk is bad. A full disk, a quota, a busy
// or full database are not: the disk answers, it just says no.
func Countable(err error) bool {
	if err == nil || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return false
	}
	m := strings.ToLower(err.Error())
	for _, s := range []string{"sqlite_full", "sqlite_busy", "database is locked", "database or disk is full", "no space left", "disk quota"} {
		if strings.Contains(m, s) {
			return false
		}
	}
	return true
}

// Watch runs probe every `every` until ctx ends and calls exit once after max
// consecutive countable failures, or silentMax consecutive ticks with no answer.
// Any success or uncountable failure resets both counts; an answered error
// resets the silent one. The probe runs in its own goroutine: one that has not answered by
// the next tick (a write stuck on a dead disk) counts as a failed tick, and no
// second probe starts while it is in flight.
func Watch(ctx context.Context, every time.Duration, max, silentMax int, probe func() error, exit func(error)) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	bad, silent := 0, 0
	var res chan error // the in-flight probe's answer, nil when none
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		var err error
		noAnswer := false
		if res == nil {
			res = make(chan error, 1)
			go func(c chan<- error) { c <- probe() }(res)
			select {
			case <-ctx.Done():
				return
			case err = <-res:
				res = nil
			case <-tk.C: // no answer within a tick
				err, noAnswer = errors.New("write probe did not answer"), true
			}
		} else {
			select {
			case err = <-res:
				res = nil
			default:
				err, noAnswer = errors.New("write probe still in flight"), true
			}
		}
		if err == nil || !Countable(err) {
			bad, silent = 0, 0
			continue
		}
		if noAnswer {
			silent++
		} else {
			bad, silent = bad+1, 0
		}
		slog.Warn("database disk: write probe failed", "consecutive", bad, "silent", silent, "err", err)
		if bad >= max || silent >= silentMax {
			exit(err)
			return
		}
	}
}
