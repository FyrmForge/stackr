{ config, lib, pkgs, ... }:
let env = "/var/lib/playground/rustfs.env"; in
lib.mkIf config.playground.mocks.s3.enable {
  # rustfs is not in nixpkgs: podman (not Docker) runs the upstream image.
  virtualisation.podman.enable = true;
  virtualisation.oci-containers.backend = "podman";
  virtualisation.oci-containers.containers.rustfs = {
    image = "rustfs/rustfs:1.0.6";
    ports = [ "9000:9000" ];
    volumes = [ "/var/lib/playground/rustfs:/data" ];
    environmentFiles = [ env ];
  };
  systemd.tmpfiles.rules = [ "d /var/lib/playground/rustfs 0755 10001 10001 -" ];
  systemd.services.rustfs-creds = {
    wantedBy = [ "podman-rustfs.service" ];
    before = [ "podman-rustfs.service" ];
    path = [ pkgs.openssl ];
    serviceConfig = { Type = "oneshot"; RemainAfterExit = true; };
    script = ''
      [ -f ${env} ] && exit 0
      umask 077
      printf 'RUSTFS_ACCESS_KEY=%s\nRUSTFS_SECRET_KEY=%s\n' \
        "$(openssl rand -hex 8)" "$(openssl rand -hex 20)" > ${env}
    '';
  };

  # the bucket stackr-backups, so a destination saves without a manual step
  systemd.services.rustfs-bucket = {
    wantedBy = [ "multi-user.target" ];
    after = [ "podman-rustfs.service" ];
    requires = [ "podman-rustfs.service" ];
    path = [ pkgs.curl ];
    serviceConfig = { Type = "oneshot"; RemainAfterExit = true; EnvironmentFile = env; };
    script = ''
      for _ in $(seq 60); do
        c=$(curl -sS -o /dev/null -w '%{http_code}' --aws-sigv4 aws:amz:us-east-1:s3 --user "$RUSTFS_ACCESS_KEY:$RUSTFS_SECRET_KEY" \
          -X PUT http://127.0.0.1:9000/stackr-backups || true)
        case $c in 200|409) exit 0 ;; esac # 409: it exists
        sleep 1
      done
      exit 1
    '';
  };
}
