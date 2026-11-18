{ pkgs, ... }:
{
  virtualisation.incus = {
    enable = true;
    package = pkgs.incus; # the LTS package is flagged insecure in 25.11
    preseed = {
      storage_pools = [{ name = "default"; driver = "dir"; }];
      profiles = [{
        name = "stackr";
        config = {
          "security.privileged" = "true";
          "security.nesting" = "true";
          "raw.lxc" = "lxc.apparmor.profile=unconfined";
        };
        devices.root = { type = "disk"; path = "/"; pool = "default"; };
      }];
    };
  };
}
