# Beta gate

Status: shipped and rig-verified 2026-09-14.

Panel archives now carry the master key and restore cleanly after a full wipe;
only server admins can enter or submit organization setup; cookie `Secure` is
derived from effective TLS instead of development mode. Tests cover archive
members, authorization and the TLS cookie matrix. Live HTTP and HTTPS paths
were both exercised on the disposable rig.
