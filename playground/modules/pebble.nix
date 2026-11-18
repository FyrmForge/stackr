{ pkgs, ... }:
let
  dir = "/var/lib/playground/pebble";
  cfg = pkgs.writeText "pebble.json" (builtins.toJSON {
    pebble = {
      listenAddress = "0.0.0.0:14000";
      managementListenAddress = "0.0.0.0:15000";
      certificate = "${dir}/api.pem";
      privateKey = "${dir}/api-key.pem";
      httpPort = 80;
      tlsPort = 443;
      ocspResponderURL = "";
      externalAccountBindingRequired = false;
    };
  });
in
{
  # One-off: stable CA + API cert for acme.stackr.test.
  systemd.services.pebble-certs = {
    wantedBy = [ "multi-user.target" ];
    before = [ "pebble.service" ];
    path = [ pkgs.openssl ];
    serviceConfig = { Type = "oneshot"; RemainAfterExit = true; };
    script = ''
      mkdir -p ${dir}; cd ${dir}
      [ -f api.pem ] && exit 0
      openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=playground pebble api ca" \
        -keyout ca-key.pem -out api-root.pem
      openssl req -newkey rsa:2048 -nodes -subj "/CN=acme.stackr.test" \
        -keyout api-key.pem -out api.csr
      printf 'subjectAltName=DNS:acme.stackr.test\nbasicConstraints=CA:FALSE\n' > ext.cnf
      openssl x509 -req -in api.csr -CA api-root.pem -CAkey ca-key.pem -CAcreateserial \
        -days 3650 -extfile ext.cnf -out api.pem
      chmod 644 api-root.pem api.pem
    '';
  };

  systemd.services.pebble = {
    wantedBy = [ "multi-user.target" ];
    after = [ "pebble-certs.service" "network-online.target" "dnsmasq.service" ];
    requires = [ "pebble-certs.service" ];
    wants = [ "network-online.target" ];
    environment.PEBBLE_VA_NOSLEEP = "1";
    serviceConfig = {
      ExecStart = "${pkgs.pebble}/bin/pebble -config ${cfg} -dnsserver 203.0.113.1:53";
      Restart = "on-failure";
    };
  };

  # Re-push Pebble's root into the nodes whenever Pebble (re)starts.
  systemd.services.playground-trust = {
    wantedBy = [ "pebble.service" ];
    partOf = [ "pebble.service" ];
    after = [ "pebble.service" "incus.service" ];
    unitConfig = {
      ConditionPathExists = "/etc/playground/nodes.sh";
      StartLimitIntervalSec = 0;
    };
    path = with pkgs; [ bash coreutils curl incus ];
    serviceConfig = {
      Type = "oneshot";
      ExecStart = "/etc/playground/nodes.sh trust";
      Restart = "on-failure"; # Pebble may not be listening yet
      RestartSec = 3;
    };
  };
}
