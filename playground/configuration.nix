{ lib, pkgs, modulesPath, ... }:
{
  imports = [
    (modulesPath + "/profiles/qemu-guest.nix")
    ./playground.nix
    ./modules/options.nix
    ./modules/network.nix
    ./modules/dns.nix
    ./modules/incus.nix
    ./modules/pebble.nix
    ./modules/s3.nix
    ./modules/disks.nix
    ./modules/shares.nix
    ./modules/namecheap.nix
    ./modules/github.nix
  ];

  # Same labels as the nixpkgs qemu-efi disk image, so the built image and
  # nixos-rebuild on the VM agree. Grows to fill the disk on boot.
  fileSystems."/" = { device = "/dev/disk/by-label/nixos"; fsType = "ext4"; autoResize = true; };
  fileSystems."/boot" = { device = "/dev/disk/by-label/ESP"; fsType = "vfat"; };
  boot.growPartition = true;
  boot.initrd.availableKernelModules = [ "virtio_pci" "virtio_blk" "virtio_scsi" "sd_mod" "sr_mod" "ahci" "xhci_pci" ];
  boot.loader.systemd-boot.enable = true;
  boot.loader.efi.canTouchEfiVariables = false; # firmware falls back to EFI/BOOT/BOOTX64.EFI
  boot.kernelModules = [
    "wireguard" "overlay" "br_netfilter" "nf_conntrack" "nf_tables" "nft_compat"
    "nf_nat" "iptable_nat" "ip6table_nat" "nfs" "cifs" "veth"
  ];
  services.qemuGuest.enable = true;

  networking.hostName = "playground";
  time.timeZone = "UTC";

  services.openssh = {
    enable = true;
    settings = {
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
      PermitRootLogin = "prohibit-password";
    };
  };
  users.users.root.openssh.authorizedKeys.keys =
    lib.filter (l: l != "" && !(lib.hasPrefix "#" l))
      (lib.splitString "\n" (builtins.readFile ./ssh-keys));

  environment.systemPackages = with pkgs; [ curl jq openssl tcpdump dig nftables ];

  systemd.tmpfiles.rules = [
    "d /var/lib/playground 0755 root root -"
  ];

  system.stateVersion = "25.11";
}
