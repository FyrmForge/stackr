package serverconfig

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
)

// Node is a node the file declares: its tags. A node is joined with the
// installer, never created by a file; a declared node that has not joined is
// a gap ("Add this node"), a joined node the file leaves out is unmanaged.
type Node struct {
	Tags []string `yaml:"tags,omitempty"`
}

// Placement is a named place envs belong to. Node is the node's name, absent
// = no node yet. A nil Open is open to all.
type Placement struct {
	Node string `yaml:"node,omitempty"`
	Open *Share `yaml:"open,omitempty"`
}

// DefaultPlacement is the reserved placement: repointed like any other,
// never removed.
const DefaultPlacement = "default"

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

var knownTags = []string{node.TagIngress, node.TagEgress, node.TagBuilder, node.TagStorage}

func (f *File) checkNodes() error {
	for name, n := range f.Nodes {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("nodes: %q is not a node name (lower-case letters, digits and -)", name)
		}
		for _, t := range n.Tags {
			if !slices.Contains(knownTags, t) {
				return fmt.Errorf("nodes.%s: %q is not a tag; use %s", name, t, strings.Join(knownTags, ", "))
			}
		}
		f.Nodes[name] = Node{Tags: slices.Compact(slices.Sorted(slices.Values(n.Tags)))}
	}
	return nil
}

func (f *File) checkPlacements() error {
	for name, p := range f.Placements {
		switch {
		case !nameRe.MatchString(name):
			return fmt.Errorf("placements: %q is not a placement name (lower-case letters, digits and -)", name)
		case p.Node != "" && !nameRe.MatchString(p.Node):
			return fmt.Errorf("placements.%s: %q is not a node name", name, p.Node)
		}
		if p.Open == nil {
			continue
		}
		for _, o := range p.Open.Orgs {
			if !slug.Valid(o) || slug.Reserved(o) {
				return fmt.Errorf("placements.%s: open: %q is not an org slug", name, o)
			}
		}
		slices.Sort(p.Open.Orgs)
	}
	return nil
}

// openText is a placement's access as the plan and the file spell it.
func openText(all bool, orgs []string) string {
	if all {
		return "all"
	}
	o := slices.Sorted(slices.Values(orgs))
	if len(o) == 0 {
		return "none"
	}
	return strings.Join(o, ", ")
}

// Access is the placement's access after the file: nil open = all.
func (p Placement) Access() (bool, []string) {
	if p.Open == nil || p.Open.All {
		return true, nil
	}
	return false, p.Open.Orgs
}

func tagText(tags []string) string {
	if len(tags) == 0 {
		return "none"
	}
	return strings.Join(slices.Sorted(slices.Values(tags)), ", ")
}

// MoveKey is the Live.Moves and Live.MoveEnvs key of a repoint.
func MoveKey(placement, node string) string { return placement + "->" + node }

// ---- plan ----

func (d *differ) nodeLive(name string) (NodeLive, bool) {
	i := slices.IndexFunc(d.live.Nodes, func(n NodeLive) bool { return n.Name == name && !n.Pending && !n.Forgotten })
	if i < 0 {
		return NodeLive{}, false
	}
	return d.live.Nodes[i], true
}

func (d *differ) forgotten(name string) bool {
	return slices.ContainsFunc(d.live.Nodes, func(n NodeLive) bool { return n.Name == name && n.Forgotten })
}

func (d *differ) placementLive(name string) (PlacementLive, bool) {
	i := slices.IndexFunc(d.live.Placements, func(p PlacementLive) bool { return p.Name == name })
	if i < 0 {
		return PlacementLive{}, false
	}
	return d.live.Placements[i], true
}

// gap adds a node gap once per node name.
func (d *differ) nodeGap(name string) {
	if slices.ContainsFunc(d.p.Gaps, func(g planfile.Gap) bool { return g.Kind == "node" && g.Key == name }) {
		return
	}
	g := planfile.Gap{Kind: "node", Key: name}
	if n, ok := d.f.Nodes[name]; ok {
		g.Tags = n.Tags
	}
	d.p.Gaps = append(d.p.Gaps, g)
}

// nodes compares the declared nodes' tags with the joined ones. A declared
// node that has not joined is a gap; a joined node the file leaves out is a
// note. Nodes are never created or removed here.
func (d *differ) nodes() {
	if d.f.Nodes == nil {
		return
	}
	used := map[string]bool{}
	for _, p := range d.f.Placements {
		used[p.Node] = true
	}
	for _, name := range slices.Sorted(maps.Keys(d.f.Nodes)) {
		fn := d.f.Nodes[name]
		n, ok := d.nodeLive(name)
		if !ok {
			d.nodeGap(name)
			if !used[name] {
				d.note("node %s is not joined yet; add it to start using it", name)
			}
			continue
		}
		if tagText(n.Tags) == tagText(fn.Tags) {
			continue
		}
		for _, t := range n.Tags {
			if !slices.Contains(fn.Tags, t) {
				if why := n.Locked[t]; why != "" {
					d.block("nodes.%s: cannot drop %s: %s", name, t, why)
				}
			}
		}
		d.add(Change{Kind: "node-tags", Tile: name, Field: "tags", Old: tagText(n.Tags), New: tagText(fn.Tags)})
	}
	for _, n := range d.live.Nodes {
		if _, declared := d.f.Nodes[n.Name]; !declared && !n.Pending && !n.Forgotten {
			d.note("%s is not in the file (unmanaged)", n.Name)
		}
	}
}

// placements compares the declared placements with the live ones: new ones,
// access, the node (a first node, or a repoint through the move checks), and
// the removal rows for the ones the file no longer names.
func (d *differ) placements() {
	if d.f.Placements == nil {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(d.f.Placements)) {
		fp := d.f.Placements[name]
		all, orgs := fp.Access()
		for _, s := range orgs {
			if !d.orgKnown(s) {
				d.block("placements.%s: open: no org %q", name, s)
			}
		}
		cur, exists := d.placementLive(name)
		_, joined := d.nodeLive(fp.Node)
		if fp.Node != "" && !joined {
			d.nodeGap(fp.Node)
		}
		if !exists {
			c := Change{Kind: "placement", Tile: name, New: cmpNode(fp.Node), Note: "open to " + openText(all, orgs)}
			d.add(c)
			if fp.Node != "" && !joined {
				d.note("%s waits for node %s", name, fp.Node)
			}
			continue
		}
		if curAll, curOrgs := cur.OpenToAll, cur.Orgs; openText(curAll, curOrgs) != openText(all, orgs) {
			d.add(Change{Kind: "placement-open", Tile: name, Field: "open", Old: openText(curAll, curOrgs), New: openText(all, orgs)})
		}
		d.placementNode(name, fp, cur, joined)
	}
	for _, cur := range d.live.Placements {
		if _, named := d.f.Placements[cur.Name]; named || cur.Name == DefaultPlacement {
			continue
		}
		c := Change{Kind: "placement-delete", Tile: cur.Name, Old: cmpNode(cur.Node), Key: "placement:" + cur.Name}
		if cur.Used > 0 {
			c.Note = fmt.Sprintf("in use by %s; refused until they move", plural(cur.Used, "pin or env"))
		}
		d.remove(c)
	}
}

func cmpNode(n string) string {
	if n == "" {
		return "no node yet"
	}
	return n
}

// placementNode plans the node of an existing placement.
func (d *differ) placementNode(name string, fp Placement, cur PlacementLive, joined bool) {
	switch {
	case fp.Node == "" || fp.Node == cur.Node:
		if fp.Node == "" && cur.Node != "" {
			d.note("%s names no node in the file; it stays on %s", name, cur.Node)
		}
		return
	case !joined:
		d.note("%s waits for node %s", name, fp.Node)
		return
	case cur.Node == "":
		d.add(Change{Kind: "placement-node", Tile: name, Field: "node", New: fp.Node})
		return
	}
	key := MoveKey(name, fp.Node)
	var failed []string
	for _, c := range d.live.Moves[key] {
		if !c.OK {
			failed = append(failed, c.Name+": "+c.Detail)
		}
	}
	if len(failed) > 0 {
		d.block("placements.%s: moving to %s fails: %s", name, fp.Node, strings.Join(failed, "; "))
		return
	}
	c := Change{Kind: "placement-move", Tile: name, Field: "node", Old: cur.Node, New: fp.Node}
	if envs := d.live.MoveEnvs[key]; len(envs) > 0 {
		verb := "moves"
		if d.forgotten(cur.Node) {
			verb = "restores from backups"
		}
		c.Impact = fmt.Sprintf("%s %s (%s) to %s; each stops while it moves", verb, plural(len(envs), "env"), envList(envs), fp.Node)
	}
	d.add(c)
}

// envList is up to three env paths, then "+N".
func envList(envs []string) string {
	if len(envs) <= 3 {
		return strings.Join(envs, ", ")
	}
	return fmt.Sprintf("%s, +%d", strings.Join(envs[:3], ", "), len(envs)-3)
}
