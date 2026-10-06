package docker

import (
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/network"
)

// envRange is the pool stackr carves its networks from, one /24 each.
// Docker's own default pools run out near 30 networks.
// ponytail: 256 networks; widen the range (or the prefix) when a box needs more.
var envRange = netip.MustParsePrefix("10.213.0.0/16")

const (
	subnetBits  = 24
	subnetTries = 5 // picks per create, when concurrent creates race for a block
)

var errNoSubnet = errors.New("no free network space in 10.213.0.0/16 for a new environment; remove one, or move the host off that range")

// EnsureNetwork creates a plain bridge network carrying labels on the first
// free /24 of envRange; one that already exists is fine (its labels and
// subnet are left as they are).
func (d *Client) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	// Every network, not a name filter: the filter is a SUBSTRING match, and
	// the free-block pick needs every subnet on the host anyway.
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return err
	}
	if slices.ContainsFunc(nets, func(n network.Summary) bool { return n.Name == name }) {
		return nil
	}
	all := map[string]string{LabelManaged: "true"}
	maps.Copy(all, labels)
	// Failing to read the host's interfaces only loses the host check; Docker
	// still refuses a block that overlaps one of its own networks.
	addrs, _ := net.InterfaceAddrs()
	return createInFreeSubnet(usedSubnets(nets, addrs), func(subnet netip.Prefix) error {
		_, err := d.cli.NetworkCreate(ctx, name, network.CreateOptions{
			Driver: "bridge",
			Labels: all,
			IPAM:   &network.IPAM{Config: []network.IPAMConfig{{Subnet: subnet.String()}}},
		})
		// A concurrent create of the same name reports "already exists";
		// both callers wanted it.
		if err != nil && (cerrdefs.IsConflict(err) || strings.Contains(err.Error(), "already exists")) {
			return nil
		}
		return err
	})
}

// usedSubnets is every prefix a new network must stay clear of: the subnets
// of all Docker networks (any driver; IPv6 and subnet-less configs are
// skipped harmlessly) and the addresses of the host's interfaces. An
// interface network at least as narrow as envRange blocks its whole subnet
// (a route conflict); a wider one (a 10.0.0.0/8 LAN, loopback) and any IPv6
// address block only the host's own address.
func usedSubnets(nets []network.Summary, addrs []net.Addr) []netip.Prefix {
	var used []netip.Prefix
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			if p, err := netip.ParsePrefix(c.Subnet); err == nil {
				used = append(used, p.Masked())
			}
		}
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if ones, _ := ipn.Mask.Size(); ip.Is4() && ones >= envRange.Bits() {
			used = append(used, netip.PrefixFrom(ip, ones).Masked())
		} else {
			used = append(used, netip.PrefixFrom(ip, ip.BitLen()))
		}
	}
	return used
}

// firstFreeSubnet is the first /24 of envRange overlapping nothing in used.
func firstFreeSubnet(used []netip.Prefix) (netip.Prefix, bool) {
	b := envRange.Addr().As4()
	base := binary.BigEndian.Uint32(b[:])
	for i := range 1 << (subnetBits - envRange.Bits()) {
		binary.BigEndian.PutUint32(b[:], base+uint32(i)<<(32-subnetBits))
		p := netip.PrefixFrom(netip.AddrFrom4(b), subnetBits)
		if !slices.ContainsFunc(used, p.Overlaps) {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// createInFreeSubnet offers create the first free block. Docker refusing it
// as an overlap (another create won the block) marks it used and tries the
// next, a few times.
func createInFreeSubnet(used []netip.Prefix, create func(netip.Prefix) error) error {
	var err error
	for range subnetTries {
		p, ok := firstFreeSubnet(used)
		if !ok {
			return errNoSubnet
		}
		if err = create(p); err == nil || !isOverlap(err) {
			return err
		}
		used = append(used, p)
	}
	return err
}

// isOverlap matches the refusals for a subnet already taken. Docker sends
// them as a plain 400, so only the text tells them apart: "invalid pool
// request: Pool overlaps with other one on this address space", the bridge
// driver's "networks have overlapping IPv4", and a gateway "Address already
// in use".
func isOverlap(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "overlap") || strings.Contains(m, "address already in use")
}

// RemoveNetwork removes a network; a missing one is fine. Docker refuses while
// any container is still attached.
func (d *Client) RemoveNetwork(ctx context.Context, name string) error {
	err := d.cli.NetworkRemove(ctx, name)
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// ListNetworks returns the names of networks carrying all of labels.
func (d *Client) ListNetworks(ctx context.Context, labels map[string]string) ([]string, error) {
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{Filters: labelFilter(labels)})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.Name)
	}
	return out, nil
}

// Gateways returns the gateway IPs of the networks carrying all of labels.
func (d *Client) Gateways(ctx context.Context, labels map[string]string) ([]string, error) {
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{Filters: labelFilter(labels)})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			if c.Gateway != "" {
				out = append(out, c.Gateway)
			}
		}
	}
	return out, nil
}

// Connect joins a container to a network with optional DNS aliases.
// Already-connected is not an error: treating it as one made a reattach
// abandon the rest of its networks halfway through.
func (d *Client) Connect(ctx context.Context, netName, containerID string, aliases []string) error {
	var cfg *network.EndpointSettings
	if len(aliases) > 0 {
		cfg = &network.EndpointSettings{Aliases: aliases}
	}
	err := d.cli.NetworkConnect(ctx, netName, containerID, cfg)
	if err != nil && (strings.Contains(err.Error(), "already exists") ||
		strings.Contains(err.Error(), "already attached")) {
		return nil
	}
	return wrap(err)
}

// Disconnect forces a running container off a network; not being on it is fine.
func (d *Client) Disconnect(ctx context.Context, netName, containerID string) error {
	err := d.cli.NetworkDisconnect(ctx, netName, containerID, true)
	if err != nil && strings.Contains(err.Error(), "is not connected") {
		return nil
	}
	return wrap(err)
}

// NetworkMembers lists the container ids on a network. A missing network is
// empty. Ids that do not inspect (docker lists the network's own
// "<net>-endpoint" sandbox among them) are skipped.
func (d *Client) NetworkMembers(ctx context.Context, name string) ([]string, error) {
	n, err := d.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for id := range n.Containers {
		if _, err := d.cli.ContainerInspect(ctx, id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// MemberAddr returns a container's IP on one network plus the network's
// subnet. Not being on the network is not an error: ip is "".
func (d *Client) MemberAddr(ctx context.Context, netName, containerID string) (ip, cidr string, err error) {
	n, err := d.cli.NetworkInspect(ctx, netName, network.InspectOptions{})
	if err != nil {
		return "", "", wrap(err)
	}
	if len(n.IPAM.Config) > 0 {
		cidr = n.IPAM.Config[0].Subnet
	}
	for id, ep := range n.Containers {
		if id == containerID || strings.HasPrefix(id, containerID) {
			// docker suffixes the NETWORK's prefix; IPAM stays the authority.
			ip, _, _ = strings.Cut(ep.IPv4Address, "/")
		}
	}
	return ip, cidr, nil
}
