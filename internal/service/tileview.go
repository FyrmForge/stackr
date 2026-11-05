package service

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// The tile's line-based columns as the API shows them. The store keeps the
// strings; these are views, parsed with the parsers a deploy uses.
type (
	// MountView is one volumes line. Source is the volume slug, the host
	// path or the share slug by Kind (volume | host | share).
	MountView struct {
		Kind     string `json:"kind"`
		Source   string `json:"source"`
		Sub      string `json:"sub,omitempty"` // share: the path inside it
		Path     string `json:"path"`          // the container path
		ReadOnly bool   `json:"read_only"`
	}
	// FileView is one files line: a repo file mounted into the container.
	FileView struct {
		Source   string `json:"source"`
		Path     string `json:"path"`
		Template bool   `json:"template"`
	}
	// PortView is one published_ports line.
	PortView struct {
		Host      int    `json:"host"`
		Container int    `json:"container"`
		Protocol  string `json:"protocol"` // tcp | udp
	}
	// DepView is one depends_on line.
	DepView struct {
		Tile      string `json:"tile"`
		Condition string `json:"condition"` // started | healthy | completed
	}
	// TileView is a tile as the API answers: the row, with its JSON and
	// line-based columns as structured JSON.
	TileView struct {
		Tile
		EnvJSON        map[string]string `json:"env_json"`
		Volumes        []MountView       `json:"volumes"`
		Files          []FileView        `json:"files"`
		PublishedPorts []PortView        `json:"published_ports"`
		DependsOn      []DepView         `json:"depends_on"`
	}
)

// NewTileView parses t's columns. A line that does not parse is left out:
// the row was checked on write.
func NewTileView(t Tile) TileView {
	v := TileView{
		Tile: t, EnvJSON: map[string]string{}, Volumes: []MountView{}, Files: []FileView{},
		PublishedPorts: []PortView{}, DependsOn: []DepView{},
	}
	_ = json.Unmarshal([]byte(t.EnvJSON), &v.EnvJSON)
	if v.EnvJSON == nil {
		v.EnvJSON = map[string]string{}
	}
	for _, l := range tile.Lines(t.Volumes) {
		if m, err := tile.ParseMount(l); err == nil {
			mv := MountView{Kind: m.Kind, Sub: m.Sub, Path: m.Path, ReadOnly: m.RO}
			mv.Source = map[string]string{tile.MountVolume: m.Volume, tile.MountHost: m.Host, tile.MountShare: m.Share}[m.Kind]
			v.Volumes = append(v.Volumes, mv)
		}
	}
	for _, l := range tile.Lines(t.Files) {
		if src, dst, err := tile.ParseFileMount(l); err == nil {
			v.Files = append(v.Files, FileView{src, dst, strings.HasSuffix(l, ":template")})
		}
	}
	for _, l := range tile.Lines(t.PublishedPorts) {
		if h, c, p, err := tile.ParsePort(l); err == nil {
			v.PublishedPorts = append(v.PublishedPorts, PortView{h, c, p})
		}
	}
	for _, l := range tile.Lines(t.DependsOn) {
		if s, c, err := tile.ParseDep(l); err == nil {
			v.DependsOn = append(v.DependsOn, DepView{s, c})
		}
	}
	return v
}

// TileViews is NewTileView over a list.
func TileViews(ts []Tile, err error) ([]TileView, error) {
	out := make([]TileView, 0, len(ts))
	for _, t := range ts {
		out = append(out, NewTileView(t))
	}
	return out, err
}

// TileColumn turns a request value for one of the tile's structured columns
// (env_json, volumes, files, published_ports, depends_on) into the string
// the store keeps. A string is taken as the stored form; a list or object
// is the structured form.
func TileColumn(field string, raw json.RawMessage) (string, error) {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		err := json.Unmarshal(raw, &s)
		return s, err
	}
	bad := func(err error) (string, error) {
		return "", errs.Invalidf(field, "%s: %v", field, err)
	}
	dec := func(v any) error {
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		return d.Decode(v)
	}
	var lines []string
	switch field {
	case "env_json":
		var m map[string]string
		if err := dec(&m); err != nil {
			return bad(err)
		}
		if m == nil {
			m = map[string]string{}
		}
		b, _ := json.Marshal(m)
		return string(b), nil
	case "volumes":
		var ms []MountView
		if err := dec(&ms); err != nil {
			return bad(err)
		}
		for _, m := range ms {
			src := m.Source
			switch m.Kind {
			case tile.MountHost:
				src = "host:" + src
			case tile.MountShare:
				src = "share:" + path.Join(src, m.Sub)
			case tile.MountVolume, "":
			default:
				return bad(fmt.Errorf("kind %q is volume, host or share", m.Kind))
			}
			l := src + ":" + m.Path
			if m.ReadOnly {
				l += ":ro"
			}
			lines = append(lines, l)
		}
	case "files":
		var fs []FileView
		if err := dec(&fs); err != nil {
			return bad(err)
		}
		for _, f := range fs {
			l := f.Source + ":" + f.Path
			if f.Template {
				l += ":template"
			}
			lines = append(lines, l)
		}
	case "published_ports":
		var ps []PortView
		if err := dec(&ps); err != nil {
			return bad(err)
		}
		for _, p := range ps {
			l := fmt.Sprintf("%d:%d", p.Host, p.Container)
			if p.Protocol != "" && p.Protocol != "tcp" {
				l += "/" + p.Protocol
			}
			lines = append(lines, l)
		}
	case "depends_on":
		var ds []DepView
		if err := dec(&ds); err != nil {
			return bad(err)
		}
		for _, d := range ds {
			l := d.Tile
			if d.Condition != "" && d.Condition != "started" {
				l += ":" + d.Condition
			}
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n"), nil
}
