{ config, lib, pkgs, ... }:
let
  fake = pkgs.buildGoModule {
    pname = "fake-namecheap";
    version = "0";
    src = ../fake-namecheap;
    vendorHash = null;
  };
in
lib.mkIf config.playground.mocks.namecheap.enable {
  systemd.services.fake-namecheap = {
    wantedBy = [ "multi-user.target" ];
    after = [ "systemd-networkd.service" "network-online.target" "dnsmasq.service" ];
    wants = [ "network-online.target" ];
    path = [ pkgs.systemd ];
    serviceConfig = {
      ExecStart = "${fake}/bin/fake-namecheap -listen 203.0.113.1:8080 -state /var/lib/playground/namecheap";
      Restart = "on-failure";
      RestartSec = 2;
    };
  };
}
