{ lib, ... }:
{
  options.playground = {
    mocks.s3.enable = lib.mkEnableOption "rustfs S3 mock";
    mocks.shares.enable = lib.mkEnableOption "NFS and Samba share mocks";
    mocks.namecheap.enable = lib.mkEnableOption "fake Namecheap API";
    mocks.github.enable = lib.mkEnableOption "fake GitHub (API, git, webhooks)";
    ext.latencyMs = lib.mkOption { type = lib.types.ints.unsigned; default = 80; };
  };
}
