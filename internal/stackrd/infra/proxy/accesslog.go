package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AccessEntry is one HTTP request that Traefik routed to a tile.
type AccessEntry struct {
	Time   time.Time
	Method string
	Path   string
	Status int
	MS     int64
	Client string
}

// accessLine mirrors the fields we use from Traefik's JSON access log.
type accessLine struct {
	StartUTC   time.Time `json:"StartUTC"`
	RouterName string    `json:"RouterName"`
	Method     string    `json:"RequestMethod"`
	Path       string    `json:"RequestPath"`
	Origin     int       `json:"OriginStatus"`
	Downstream int       `json:"DownstreamStatus"`
	Duration   int64     `json:"Duration"` // ns
	ClientHost string    `json:"ClientHost"`
}

// AccessLog returns the tile's most recent proxied requests, newest first.
// scans the last 2MB of the shared access log, indexing per tile
// can come with the proper logs viewer.
func (p *Proxy) AccessLog(tileID string, limit int) []AccessEntry {
	f, err := os.Open(filepath.Join(p.dir, "access.log"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	const window = 2 << 20
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	if fi.Size() > window {
		if _, err := f.Seek(fi.Size()-window, 0); err != nil {
			return nil
		}
	}
	buf := make([]byte, window)
	n, _ := f.Read(buf)
	lines := bytes.Split(buf[:n], []byte("\n"))

	prefix := "app-" + tileID[:8] + "-"
	var out []AccessEntry
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		var l accessLine
		if err := json.Unmarshal(lines[i], &l); err != nil {
			continue
		}
		if !strings.HasPrefix(l.RouterName, prefix) {
			continue
		}
		status := l.Origin
		if status == 0 {
			status = l.Downstream
		}
		out = append(out, AccessEntry{
			Time:   l.StartUTC,
			Method: l.Method,
			Path:   l.Path,
			Status: status,
			MS:     l.Duration / int64(time.Millisecond),
			Client: l.ClientHost,
		})
	}
	return out
}
