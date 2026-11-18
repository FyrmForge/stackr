{ pkgs, ... }:
{
  # Loop-mount every disk image at the same path without .img, so incus can bind it.
  # Idempotent and no ExecStop: unmounting would pull disks out from under running
  # containers. nodes.sh disk add mounts new images itself.
  systemd.services.playground-disks = {
    wantedBy = [ "multi-user.target" ];
    before = [ "incus.service" ];
    path = [ pkgs.util-linux ];
    serviceConfig = { Type = "oneshot"; RemainAfterExit = true; };
    script = ''
      for img in /var/lib/playground/disks/*.img; do
        [ -f "$img" ] || continue
        m=''${img%.img}
        mkdir -p "$m"
        mountpoint -q "$m" || mount -o loop "$img" "$m"
      done
    '';
  };
}
