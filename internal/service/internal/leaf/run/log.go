package run

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func (l *Leaf) LogPath(r store.Run) string { return filepath.Join(l.dir, r.TileID, r.ID+".log") }

// Log is a run's log file for writing: it keeps the last LogCap bytes. The
// file may reach twice the cap between trims, never more.
type Log struct {
	f *os.File
	n int64
}

// OpenLog creates the run's log file.
func (l *Leaf) OpenLog(r store.Run) (*Log, error) {
	if err := os.MkdirAll(filepath.Join(l.dir, r.TileID), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(l.LogPath(r), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, err
	}
	return &Log{f: f}, nil
}

func (g *Log) Write(p []byte) (int, error) {
	n, err := g.f.Write(p)
	g.n += int64(n)
	if err == nil && g.n > 2*LogCap {
		err = g.trim()
	}
	return n, err
}

// trim keeps the last LogCap bytes.
func (g *Log) trim() error {
	buf := make([]byte, LogCap)
	if _, err := g.f.ReadAt(buf, g.n-LogCap); err != nil {
		return err
	}
	if err := g.f.Truncate(0); err != nil {
		return err
	}
	if _, err := g.f.WriteAt(buf, 0); err != nil {
		return err
	}
	g.n = LogCap
	_, err := g.f.Seek(LogCap, io.SeekStart)
	return err
}

func (g *Log) Close() error {
	var err error
	if g.n > LogCap {
		err = g.trim()
	}
	if cerr := g.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Tail is the last n lines of the run's log (n ≤ 0 = all). A run that has
// not written yet has an empty log.
func (l *Leaf) Tail(r store.Run, n int) (string, error) {
	b, err := l.read(r)
	return tailOf(b, n), err
}

func (l *Leaf) read(r store.Run) ([]byte, error) {
	b, err := os.ReadFile(l.LogPath(r))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}

func tailOf(b []byte, n int) string {
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "")
}

// Follow streams the run's log: the last tail lines, then new lines as the
// worker writes them, until done reports the run closed (after one last
// read) or stop is called.
func (l *Leaf) Follow(r store.Run, tail int, done func() bool) (<-chan string, func()) {
	out := make(chan string)
	ctx, stop := context.WithCancel(context.Background())
	go func() {
		defer close(out)
		b, err := l.read(r)
		if err != nil {
			return
		}
		first, off := tailOf(b, tail), int64(len(b))
		send := func(s string) bool {
			select {
			case out <- s:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, line := range strings.SplitAfter(first, "\n") {
			if line != "" && !send(strings.TrimSuffix(line, "\n")) {
				return
			}
		}
		var part string
		for {
			finished := done()
			if sz := size(l.LogPath(r)); sz < off {
				off = 0 // trimmed under us: start over from the kept tail
			}
			if f, err := os.Open(l.LogPath(r)); err == nil {
				_, _ = f.Seek(off, io.SeekStart)
				rd := bufio.NewReader(f)
				for {
					s, err := rd.ReadString('\n')
					off += int64(len(s))
					part += s
					if err != nil {
						break
					}
					if !send(strings.TrimSuffix(part, "\n")) {
						_ = f.Close()
						return
					}
					part = ""
				}
				_ = f.Close()
			}
			if finished {
				if part != "" {
					send(part)
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
	}()
	return out, stop
}

func size(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}
