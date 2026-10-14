package service

// What a member who is not a server admin sees of the cluster: names, tags,
// states and counts, never addresses or ranges.

// Redacted drops the node's addresses and range.
func (n NodeOut) Redacted() NodeOut {
	n.Range, n.WGAddress = "", ""
	return n
}

// Redacted drops the admin-only parts of a node's detail.
func (d NodeInfo) Redacted() NodeInfo {
	d.NodeOut, d.Facts, d.Cert = d.NodeOut.Redacted(), nil, nil
	return d
}

// Redacted drops the admin-only parts of the cluster view.
func (c ClusterOut) Redacted() ClusterOut {
	ns := make([]ClusterStatusNode, len(c.Nodes))
	for i, n := range c.Nodes {
		n.NodeOut, n.Public = n.Redacted(), []string{}
		ns[i] = n
	}
	c.Nodes = ns
	return c
}
