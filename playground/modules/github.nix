{ config, lib, pkgs, ... }:
let
  dir = "/var/lib/playground/github";
  pebble = "/var/lib/playground/pebble";
  fake = pkgs.buildGoModule {
    pname = "fake-github";
    version = "0";
    src = ../fake-github;
    vendorHash = null;
    doCheck = false; # its test shells out to git
  };
in
lib.mkIf config.playground.mocks.github.enable {
  # One-off: a github.stackr.test certificate signed by the Pebble API CA.
  systemd.services.github-certs = {
    wantedBy = [ "multi-user.target" ];
    after = [ "pebble-certs.service" ];
    requires = [ "pebble-certs.service" ];
    path = [ pkgs.openssl ];
    serviceConfig = { Type = "oneshot"; RemainAfterExit = true; };
    script = ''
      mkdir -p ${dir}; cd ${dir}
      [ -f github.pem ] && exit 0
      openssl req -newkey rsa:2048 -nodes -subj "/CN=github.stackr.test" \
        -keyout github-key.pem -out github.csr
      printf 'subjectAltName=DNS:github.stackr.test\nbasicConstraints=CA:FALSE\n' > ext.cnf
      openssl x509 -req -in github.csr -CA ${pebble}/api-root.pem -CAkey ${pebble}/ca-key.pem \
        -CAcreateserial -days 3650 -extfile ext.cnf -out github.pem
      chmod 644 github.pem
    '';
  };

  systemd.services.fake-github = {
    wantedBy = [ "multi-user.target" ];
    after = [ "systemd-networkd.service" "network-online.target" "github-certs.service" ];
    requires = [ "github-certs.service" ];
    wants = [ "network-online.target" ];
    path = [ pkgs.git ];
    serviceConfig = {
      ExecStart = "${fake}/bin/fake-github -listen 0.0.0.0:8443 -state ${dir} -cert ${dir}/github.pem -key ${dir}/github-key.pem -url https://github.stackr.test:8443";
      Restart = "on-failure";
      RestartSec = 2;
    };
  };
}
