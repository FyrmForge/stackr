{ config, lib, pkgs, ... }:
let
  ms = config.playground.ext.latencyMs;
in
{
  networking.useNetworkd = true;
  networking.useDHCP = false;
  networking.firewall.enable = false; # lab VM; nftables below only does NAT
  networking.nftables.enable = true;   # Incus module also enables this

  boot.kernel.sysctl = {
    "net.ipv4.ip_forward" = 1;
    "net.ipv4.conf.all.forwarding" = lib.mkForce 1; # NixOS defaults this to 0, which undoes ip_forward
    # br_netfilter would DNAT hairpinned lan0 traffic at layer 2, skipping the masquerade
    "net.bridge.bridge-nf-call-iptables" = 0;
    "net.bridge.bridge-nf-call-ip6tables" = 0;
  };

  systemd.network.enable = true;
  systemd.network.netdevs = {
    "20-lan0".netdevConfig = { Kind = "bridge"; Name = "lan0"; };
    "20-wan0".netdevConfig = { Kind = "bridge"; Name = "wan0"; };
  };
  systemd.network.networks = {
    # Uplink: any virtio NIC that is not a bridge or container veth.
    "10-uplink" = {
      matchConfig.Name = "en* eth*";
      networkConfig.DHCP = "ipv4";
      linkConfig.RequiredForOnline = "routable";
    };
    "20-lan0" = {
      matchConfig.Name = "lan0";
      address = [ "10.77.0.1/24" ];
      networkConfig.ConfigureWithoutCarrier = true;
      linkConfig.RequiredForOnline = "no";
    };
    "20-wan0" = {
      matchConfig.Name = "wan0";
      address = [ "203.0.113.1/24" ];
      networkConfig.ConfigureWithoutCarrier = true;
      linkConfig.RequiredForOnline = "no";
    };
  };

  # Uplink masquerade, DNAT 80/443 to ctrl, hairpin.
  networking.nftables.tables.playground = {
    family = "ip";
    content = ''
      chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
        ip daddr 203.0.113.1 tcp dport { 80, 443 } dnat to 10.77.0.10
        iifname != { "lan0", "wan0", "lo" } fib daddr type local tcp dport { 80, 443 } dnat to 10.77.0.10
        # laptop-reachable ext ingress
        iifname != { "lan0", "wan0", "lo" } tcp dport 20080 dnat to 203.0.113.20:80
        iifname != { "lan0", "wan0", "lo" } tcp dport 20443 dnat to 203.0.113.20:443
      }
      chain output {
        type nat hook output priority -100; policy accept;
        fib daddr type local ip daddr != 127.0.0.0/8 tcp dport { 80, 443 } dnat to 10.77.0.10
      }
      chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        # hairpin only: lan clients and ctrl reaching ctrl via the DNAT. ext and the VM
        # (Pebble) keep their source; ctrl answers them through its gateway, the VM.
        ct status dnat ip saddr 10.77.0.0/24 ip daddr 10.77.0.10 tcp dport { 80, 443 } masquerade
        ip saddr { 10.77.0.0/24, 203.0.113.0/24 } oifname != { "lan0", "wan0" } masquerade
        ip saddr 10.77.0.0/24 oifname "wan0" masquerade
      }
    '';
  };

  # Latency on wan0 (applies to traffic the VM sends onto wan0).
  systemd.services.wan0-netem = {
    wantedBy = [ "multi-user.target" ];
    after = [ "sys-subsystem-net-devices-wan0.device" "systemd-networkd.service" ];
    bindsTo = [ "sys-subsystem-net-devices-wan0.device" ];
    serviceConfig = { Type = "oneshot"; RemainAfterExit = true; };
    path = [ pkgs.iproute2 ];
    script = ''
      tc qdisc replace dev wan0 root netem delay ${toString ms}ms
    '';
  };
}
