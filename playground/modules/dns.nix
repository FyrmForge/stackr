{ ... }:
{
  services.dnsmasq = {
    enable = true;
    resolveLocalQueries = true;
    settings = {
      interface = [ "lo" "lan0" "wan0" ];
      bind-interfaces = true;
      no-resolv = false;
      address = "/stackr.test/203.0.113.1";
      # SOA and NS so certmagic can find the zone for DNS-01
      dns-rr = [
        "stackr.test,6,026e7306737461636b720474657374000a686f73746d617374657206737461636b720474657374000000000100000e1000000258000151800000003c"
        "stackr.test,2,026e7306737461636b72047465737400"
      ];
      conf-file = "/var/lib/playground/namecheap/dnsmasq.conf";
    };
  };
  # fake Namecheap restarts dnsmasq on every record write; bursts must not trip the start limit
  systemd.services.dnsmasq.startLimitIntervalSec = 0;
  systemd.tmpfiles.rules = [
    "d /var/lib/playground/namecheap 0755 root root -"
    "f /var/lib/playground/namecheap/dnsmasq.conf 0644 root root -"
  ];
}
