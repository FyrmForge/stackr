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
// consecutive countable failures. Any success or uncountable failure resets
// the count.
func Watch(ctx context.Context, every time.Duration, max int, probe func() error, exit func(error)) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	bad := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		err := probe()
		if err == nil || !Countable(err) {
			bad = 0
			continue
		}
		bad++
		slog.Warn("database disk: write probe failed", "consecutive", bad, "err", err)
		if bad >= max {
			exit(err)
			return
		}
	}
}
