{ config, lib, pkgs, ... }:
let pass = "/var/lib/playground/samba.pass"; in
lib.mkIf config.playground.mocks.shares.enable {
  users.users.stackr = { isSystemUser = true; group = "stackr"; };
  users.groups.stackr = { };

  systemd.tmpfiles.rules = [
    "d /srv/shares/nfs 0777 root root -"
    "d /srv/shares/smb 0777 root root -"
  ];

  services.nfs.server = {
    enable = true;
    exports = ''
      /srv/shares/nfs 10.77.0.0/24(rw,sync,no_subtree_check,no_root_squash) 203.0.113.0/24(rw,sync,no_subtree_check,no_root_squash)
    '';
  };

  services.samba = {
    enable = true;
    settings = {
      global = { "server role" = "standalone server"; "map to guest" = "never"; };
      playground = {
        path = "/srv/shares/smb";
        "read only" = "no";
        "valid users" = "stackr";
        "force user" = "root";
      };
    };
  };

  systemd.services.samba-password = {
    wantedBy = [ "multi-user.target" ];
    before = [ "samba-smbd.service" ];
    after = [ "samba-nmbd.service" ];
    path = [ pkgs.openssl config.services.samba.package ];
    serviceConfig = { Type = "oneshot"; RemainAfterExit = true; };
    script = ''
      if [ ! -f ${pass} ]; then umask 077; openssl rand -hex 12 > ${pass}; fi
      (cat ${pass}; cat ${pass}) | smbpasswd -s -a stackr
    '';
  };
}
