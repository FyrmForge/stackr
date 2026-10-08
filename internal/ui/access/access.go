// Package access is how the UI reads elevated access: the "<tile> <perm>"
// grant lines (internal/service/internal/leaf/hostgrant owns the grammar;
// the web cannot import it, so the prefixes are repeated here), their
// labels, the strongest one a tile holds, and the approve form the stack
// drawer and the admin tab share.
package access

import (
	"net/url"
	"slices"
	"strings"
)

// Kinds, strongest first (the order of Rank).
const (
	KindNet        = "network"
	KindDocker     = "docker"
	KindPrivileged = "privileged"
	KindDevice     = "device"
	KindLAN        = "lan"
	KindPort       = "port"
	KindFolder     = "folder"
	KindOther      = "other"
)

var rank = []string{KindNet, KindDocker, KindPrivileged, KindDevice, KindLAN, KindPort, KindFolder, KindOther}

// Perm is one grant line read for display.
type Perm struct {
	Line    string // "<tile> <perm>", what is posted back
	Tile    string
	Kind    string
	Label   string // "Host folder", "Docker socket", ...
	Chip    string // the card chip: "host net", "docker", ...
	Detail  string // the path, port, address; "" = none
	Waiting bool   // asked for, not granted yet
}

// Risky perms give full trust on the server: they need the stack's name
// typed to approve.
func (p Perm) Risky() bool { return p.Kind == KindNet || p.Kind == KindDocker }

// Parse reads one grant line.
func Parse(line string) Perm {
	tile, perm, _ := strings.Cut(line, " ")
	p := Perm{Line: line, Tile: tile, Kind: KindOther, Label: perm, Chip: perm}
	switch {
	case perm == "network:host":
		p.Kind, p.Label, p.Chip = KindNet, "Host networking", "host net"
	case perm == "privileged":
		p.Kind, p.Label, p.Chip = KindPrivileged, "Privileged", "privileged"
	case strings.HasPrefix(perm, "device:"):
		p.Kind, p.Label, p.Chip, p.Detail = KindDevice, "Device", "device", perm[len("device:"):]
	case strings.HasPrefix(perm, "lan:"):
		p.Kind, p.Label, p.Chip, p.Detail = KindLAN, "LAN access", "lan", perm[len("lan:"):]
		if p.Detail == "all" {
			p.Detail = "all of the LAN"
		}
	case strings.HasPrefix(perm, "port:"):
		p.Kind, p.Label, p.Chip, p.Detail = KindPort, "Server port", "port", perm[len("port:"):]
	case isDockerSock(perm):
		p.Kind, p.Label, p.Chip, p.Detail = KindDocker, "Docker socket", "docker", "/var/run/docker.sock"
	case strings.HasPrefix(perm, "host:"):
		p.Kind, p.Label, p.Chip, p.Detail = KindFolder, "Host folder", "folder", perm[len("host:"):]
	}
	return p
}

// isDockerSock: a host: perm whose source is the docker socket, or a folder
// holding it (/, /var, /var/run, /run), which gives the same trust.
func isDockerSock(perm string) bool {
	src, ok := strings.CutPrefix(perm, "host:")
	if !ok {
		return false
	}
	src, _, _ = strings.Cut(src, ":")
	if src == "/" {
		return true
	}
	src = strings.TrimSuffix(src, "/")
	for _, sock := range []string{"/var/run/docker.sock", "/run/docker.sock"} {
		if strings.HasPrefix(sock, src) && (len(sock) == len(src) || sock[len(src)] == '/') || strings.HasPrefix(src, sock) {
			return true
		}
	}
	return false
}

// Stronger orders perms: the one that gives the most first.
func Stronger(a, b Perm) int {
	return slices.Index(rank, a.Kind) - slices.Index(rank, b.Kind)
}

// Strongest is the strongest permission among the lines of one tile; ok is
// false when it has none.
func Strongest(lines []string, tile string) (best Perm, ok bool) {
	for _, l := range lines {
		if p := Parse(l); p.Tile == tile && (!ok || Stronger(p, best) < 0) {
			best, ok = p, true
		}
	}
	return best, ok
}

// Waits reports whether any pending line is the tile's.
func Waits(pending []string, tile string) bool {
	return slices.ContainsFunc(pending, func(l string) bool { return Parse(l).Tile == tile })
}

// TilePerms is one tile's permissions, strongest first.
type TilePerms struct {
	Tile  string
	Perms []Perm
}

// ForTile is the tile's granted and waiting perms, strongest first.
func ForTile(granted, pending []string, tile string) []Perm {
	for _, g := range Group(granted, pending) {
		if g.Tile == tile {
			return g.Perms
		}
	}
	return nil
}

// Group sorts lines under their tile, in tile order; pending lines are
// marked Waiting.
func Group(granted, pending []string) []TilePerms {
	var ps []Perm
	for _, l := range granted {
		ps = append(ps, Parse(l))
	}
	for _, l := range pending {
		p := Parse(l)
		p.Waiting = true
		ps = append(ps, p)
	}
	slices.SortStableFunc(ps, func(a, b Perm) int {
		if c := strings.Compare(a.Tile, b.Tile); c != 0 {
			return c
		}
		return Stronger(a, b)
	})
	var out []TilePerms
	for _, p := range ps {
		if n := len(out); n == 0 || out[n-1].Tile != p.Tile {
			out = append(out, TilePerms{Tile: p.Tile})
		}
		out[len(out)-1].Perms = append(out[len(out)-1].Perms, p)
	}
	return out
}

// NeedName says a grant holds a perm that needs the stack's name typed.
func NeedName(lines []string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return Parse(l).Risky() })
}

// Refusals the approve handlers show.
const (
	NoneTicked = "Tick at least one permission."
	NameWrong  = "Type the stack name to approve host networking or the docker socket."
)

// Read is the approve form as posted: the pending set that was shown and
// the lines ticked. msg is a refusal in plain words ("" = go ahead); an
// empty grant would mean "all" to the service, so it is refused here.
// stack is the name to type when a full trust perm is ticked.
func Read(f url.Values, stack string) (pending, grant []string, msg string) {
	split := func(vs []string) (out []string) {
		for _, v := range vs {
			for l := range strings.SplitSeq(v, "\n") {
				if l = strings.TrimSpace(l); l != "" {
					out = append(out, l)
				}
			}
		}
		return out
	}
	pending, grant = split(f["pending"]), split(f["grant"])
	switch {
	case len(grant) == 0:
		msg = NoneTicked
	case NeedName(grant) && strings.TrimSpace(f.Get("confirm")) != stack:
		msg = NameWrong
	}
	return pending, grant, msg
}
