package docker

import (
	"context"
	"maps"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
)

// EnsureNetwork creates a plain bridge network carrying labels; one that
// already exists is fine (its labels are left as they are).
func (d *Client) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	// The name filter is a SUBSTRING match, so compare exactly.
	nets, err := d.cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("name", name))})
	if err != nil {
		return err
	}
	for _, n := range nets {
		if n.Name == name {
			return nil
		}
	}
	all := map[string]string{LabelManaged: "true"}
	maps.Copy(all, labels)
	_, err = d.cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge", Labels: all})
	// A concurrent create reports "already exists"; both callers wanted it.
	if err != nil && (cerrdefs.IsConflict(err) || strings.Contains(err.Error(), "already exists")) {
		return nil
	}
	return err
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
	if err != nil && (strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "already attached")) {
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
