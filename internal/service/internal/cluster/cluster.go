// Package cluster holds the cluster address plan: the constants every layer
// that touches node addresses shares.
//
// ponytail: lane W6 adds internal/mesh with these same constants; delete this
// package then and import mesh from leaf/node and vip.
package cluster

import "net/netip"

const (
	// Supernet holds one /16 per node. Aligned, so one rule covers the lot.
	Supernet = "10.208.0.0/12"
	// Transit is the WireGuard link net: outside the supernet and out of
	// 100.64/10 (the CGNAT range Tailscale uses).
	Transit = "10.207.255.0/24"
	// ControllerRange is the controller's /16, the range a single-node
	// install has always used.
	ControllerRange = "10.213.0.0/16"
)

// SupernetPrefix and TransitPrefix are the constants parsed.
var (
	SupernetPrefix = netip.MustParsePrefix(Supernet)
	TransitPrefix  = netip.MustParsePrefix(Transit)
)
