//go:build linux

package wgapply

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Kernel is the real Applier: netlink for the link, address and routes,
// wgctrl for keys and peers. Needs CAP_NET_ADMIN.
type Kernel struct{}

var _ Applier = Kernel{}

func (Kernel) Apply(cfg Config) error {
	link, err := netlink.LinkByName(cfg.Iface)
	var nf netlink.LinkNotFoundError
	if errors.As(err, &nf) {
		link = &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: cfg.Iface}, LinkType: "wireguard"}
		if err = netlink.LinkAdd(link); err == nil {
			link, err = netlink.LinkByName(cfg.Iface)
		}
	}
	if err != nil {
		return fmt.Errorf("wgapply: link %s: %w", cfg.Iface, err)
	}
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: ipNet(cfg.Address)}); err != nil {
		return fmt.Errorf("wgapply: address: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("wgapply: link up: %w", err)
	}
	if err := configurePeers(cfg); err != nil {
		return err
	}
	return syncRoutes(link, cfg)
}

func configurePeers(cfg Config) error {
	c, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer c.Close()
	key, err := wgtypes.ParseKey(cfg.PrivateKey)
	if err != nil {
		return fmt.Errorf("wgapply: private key: %w", err)
	}
	dev, err := c.Device(cfg.Iface)
	if err != nil {
		return err
	}
	want := map[wgtypes.Key]bool{}
	wc := wgtypes.Config{PrivateKey: &key, ListenPort: &cfg.ListenPort}
	keep := 25 * time.Second
	for _, p := range cfg.Peers {
		pk, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			return fmt.Errorf("wgapply: peer %s key: %w", p.NodeID, err)
		}
		want[pk] = true
		pc := wgtypes.PeerConfig{PublicKey: pk, ReplaceAllowedIPs: true, PersistentKeepaliveInterval: &keep}
		for _, a := range p.AllowedIPs {
			pc.AllowedIPs = append(pc.AllowedIPs, *ipNet(a))
		}
		// An existing peer keeps its current endpoint, so a repeated push does
		// not undo a failover move. One added before its addresses were known
		// has none and takes the first candidate once there is one.
		hasEndpoint := false
		for _, d := range dev.Peers {
			hasEndpoint = hasEndpoint || d.PublicKey == pk && d.Endpoint != nil
		}
		if !hasEndpoint && len(p.Candidates) > 0 {
			pc.Endpoint = net.UDPAddrFromAddrPort(p.Candidates[0])
		}
		wc.Peers = append(wc.Peers, pc)
	}
	for _, d := range dev.Peers {
		if !want[d.PublicKey] {
			wc.Peers = append(wc.Peers, wgtypes.PeerConfig{PublicKey: d.PublicKey, Remove: true})
		}
	}
	return c.ConfigureDevice(cfg.Iface, wc)
}

func syncRoutes(link netlink.Link, cfg Config) error {
	want := map[string]bool{ipNet(cfg.Address.Masked()).String(): true}
	for _, p := range cfg.Peers {
		if len(p.AllowedIPs) == 0 {
			continue
		}
		r := p.AllowedIPs[0] // the node range; the transit /32 rides the connected route
		want[ipNet(r).String()] = true
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: ipNet(r), Scope: netlink.SCOPE_LINK}); err != nil {
			return fmt.Errorf("wgapply: route %s: %w", r, err)
		}
	}
	have, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range have {
		if r.Dst != nil && !want[r.Dst.String()] {
			if err := netlink.RouteDel(&r); err != nil {
				return fmt.Errorf("wgapply: drop route %s: %w", r.Dst, err)
			}
		}
	}
	return nil
}

func (Kernel) Handshakes(iface string) (map[string]time.Time, error) {
	c, err := wgctrl.New()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	dev, err := c.Device(iface)
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for _, p := range dev.Peers {
		out[p.PublicKey.String()] = p.LastHandshakeTime
	}
	return out, nil
}

func (Kernel) Seen(iface string) (map[string]netip.AddrPort, error) {
	c, err := wgctrl.New()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	dev, err := c.Device(iface)
	if err != nil {
		return nil, err
	}
	out := map[string]netip.AddrPort{}
	for _, p := range dev.Peers {
		if p.Endpoint != nil && time.Since(p.LastHandshakeTime) < StaleAfter {
			out[p.PublicKey.String()] = p.Endpoint.AddrPort()
		}
	}
	return out, nil
}

func (Kernel) SetEndpoint(iface, publicKey string, ep netip.AddrPort) error {
	pk, err := wgtypes.ParseKey(publicKey)
	if err != nil {
		return err
	}
	c, err := wgctrl.New()
	if err != nil {
		return err
	}
	defer c.Close()
	return c.ConfigureDevice(iface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{
		PublicKey: pk, UpdateOnly: true, Endpoint: net.UDPAddrFromAddrPort(ep),
	}}})
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}
