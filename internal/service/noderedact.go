package service

import "fmt"

// What a caller who is not a server admin may be shown of the cluster. Node
// views are admin-only since placements (D3); these stay as the fallback for
// a surface that must show a node anyway: names, tags, states and counts,
// never addresses, ranges, builds, placements or other orgs' envs.

// Redacted drops the node's addresses and range, the placements on it, its
// build, size and architecture: all of it fingerprints the box or names
// orgs the caller may not be in.
func (n NodeOut) Redacted() NodeOut {
	n.Range, n.WGAddress = "", ""
	n.Placements, n.DefaultExit = []string{}, []string{}
	n.Version, n.Arch, n.Docker = "", "", ""
	n.Behind, n.TooOld, n.Warnings = false, false, []NodeWarning{}
	n.CPU, n.MemTotal, n.DiskFree, n.DiskTotal = 0, 0, 0, 0
	return n
}

// Redacted drops the admin-only parts of a node's detail. Env paths, volume
// names and the scopes that use the node name other orgs, so they go too.
func (d NodeInfo) Redacted() NodeInfo {
	d.NodeOut, d.Facts, d.Cert = d.NodeOut.Redacted(), nil, nil
	d.EnvList, d.ExitFor, d.IngressFor = []NodeEnvOut{}, []string{}, []string{}
	return d
}

// Redacted drops the admin-only parts of the cluster view. A group is named
// by its LAN range, so it gets a neutral name.
func (c ClusterOut) Redacted() ClusterOut {
	ns := make([]ClusterStatusNode, len(c.Nodes))
	for i, n := range c.Nodes {
		n.NodeOut, n.Public = n.Redacted(), []string{}
		ns[i] = n
	}
	c.Nodes = ns
	gs := make([]ClusterGroup, len(c.Groups))
	for i, g := range c.Groups {
		if g.Name != "internet" {
			g.Name = fmt.Sprintf("lan-%d", i+1)
		}
		gs[i] = g
	}
	c.Groups = gs
	return c
}
