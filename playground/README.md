# stackr playground

One NixOS VM runs everything: Incus containers `ctrl`, `w1`, `ext`, plus Pebble
(ACME), DNS for `*.stackr.test`, and optional mocks (S3, NFS/SMB, Namecheap,
fake GitHub). The NixOS config is in this folder; `nodes.sh` builds the
stackr cluster inside it.

## 1. Make the VM

Laptop (libvirt, needs Docker with /dev/kvm):

    playground/vm.sh bundle     # builds this checkout into $PLAYGROUND_OUT (default ../stackr-playground-out)
    playground/vm.sh create

`bundle` runs `make release` for `v<latest tag, patch+1>-dev.<UTC stamp>` and
leaves `images.tar` (the stackr and stackr-observe images), the installer and a
`VERSION` file in `PLAYGROUND_OUT`. It drops the previous dev image tags.
`create` runs `bundle` when there is none, builds a UEFI qcow2 of the NixOS
config inside Docker (`nixos/nix`, cached in the volume `playground-nix`; free
it with `docker volume rm playground-nix`), adds your `~/.ssh/id_ed25519.pub` to
the VM's authorized keys (the repo copy of `ssh-keys` stays a placeholder),
boots it (6 vCPU, 8 GB, 60 GB, MAC 52:54:00:77:00:01), copies this folder to
`/etc/playground` and the bundle to `/root/out`, and runs `nodes.sh up`. The
admin password is `UNIVERSAL_STACKR_PASSWORD` from `PLAYGROUND_ENV` (default
the repo's `.env`), sent over ssh stdin into `/root/.playground.env`
for `up` and `reset` only, and removed afterwards. Staging files live in
`~/.cache/stackr-playground`.

Dev loop: edit stackr, then

    playground/vm.sh bundle
    playground/vm.sh deploy

`deploy` pushes the bundle to `/root/out`, loads the images on all three nodes
and re-runs `stackr-install` on ctrl (its upgrade path; no password). After
editing the NixOS files: `playground/vm.sh sync`.

The install prints a recovery key; it is saved to
`/var/lib/playground/recovery.key` on the VM (mode 600) and never shown.

Proxmox: build the qcow2 the same way (or take it from a laptop build), copy it
to the node, and import it into a VM darthvader makes (OVMF/UEFI, virtio disk
and NIC, 6 vCPU, 8 GB, no disk yet):

    qm importdisk <vmid> nixos.qcow2 <storage>
    # attach it as virtio0, set it first in the boot order, resize to 60G

Then ssh in as root, copy this folder to `/etc/playground`, and continue below.

## 2. Entry points

On the laptop: `vm.sh ssh [cmd]`, `vm.sh nodes <args>` (runs `nodes.sh` on the
VM), `start stop status ip console`, `delete [--yes]`. On the VM, `nodes.sh`:

    up [--version vX.Y.Z | --local DIR]   default: the bundle in /root/out
    upgrade [--local DIR]       the installer's upgrade path, from a bundle
    stk ARGS                    the stkr CLI against the panel, as the admin
    x NODE CMD...               run a command in a node
    logs NODE [CONTAINER] [LINES]   a container's log; without CONTAINER, the list
    status                      one ok|FAIL line per check, exit 1 on any FAIL
    hosts | pr ...              laptop /etc/hosts lines; fake pull_request events

`status` checks the VM units, the three containers, DNS for the panel, panel
health over https with the live Pebble root (no `-k`), that each node holds the
current root, the panel image, `stk node ls`, the `dev.env` keys and mounted
disks.

New nodes join with tags: `w1` builder, `ext` ingress. Tags apply to new joins
only; an existing node keeps its tags until `reset` of that node.

## 3. Reset

    nodes.sh reset                nodes only: delete the containers (and their disks), up again
    nodes.sh reset w1|ext         one node: stk node rm, delete, relaunch, join
    nodes.sh reset ctrl           everything: same as plain reset
    nodes.sh reset --fakes [NODE] the same, and also wipe fake Namecheap, GitHub repos and apps, rustfs
                                  data (the bucket is made again) and share contents

Credentials, the Pebble CA and the GitHub cert are kept, and Pebble is not
restarted. `up` skips what exists, so rerunning it is safe.

## 4. Disks

    nodes.sh disk add w1 d1 [2G]      an ext4 loop disk, at /mnt/d1 in w1
    nodes.sh disk rm w1 d1

Images are `/var/lib/playground/disks/<node>-<name>.img`, mounted on the VM at
the same path without `.img`; the unit `playground-disks` mounts them again at
boot. Inside the node it is a separate filesystem (`stat -c %d / /mnt/d1`).

## Toggles

Edit `playground/playground.nix` (mocks, ext latency), then:

    playground/vm.sh sync

`dev.env` always points GitHub and Namecheap at the mocks, so a mock that is
off makes its connector unavailable (it does not fall back to the real service).

## Laptop access

    playground/vm.sh nodes hosts

Paste the output into `/etc/hosts` on the laptop. Tiles need one line per name
(or use dnsmasq on the laptop with `address=/stackr.test/<VM ip>`). No route or
libvirt change is needed: the VM has a fixed MAC, so the DHCP lease is stable.

Agents driving a browser (Playwright) skip /etc/hosts with the launch arg
`--host-resolver-rules=MAP *.stackr.test <vm ip>`.

## Browser trust (optional)

Pebble signs the site certificates with a root that changes on every Pebble
restart. To stop browser warnings, import it as an authority:

    curl -k https://acme.stackr.test:15000/roots/0 -o pebble-root.pem

Re-import after a Pebble restart, and run `nodes.sh trust` for the containers.

## GitHub

`playground.mocks.github.enable` runs a fake GitHub at
`https://github.stackr.test:8443` (certificate signed by the Pebble API CA).
Import `/var/lib/playground/pebble/api-root.pem` into the browser as an
authority, and add the `hosts` line for `github.stackr.test`. The GitHub
connector then runs its normal flow against it: create the app, install it
(all repos are visible), pick a repo.

Repos are created on first push, any owner and name:

    git -c http.sslCAInfo=api-root.pem push https://dev:x@github.stackr.test:8443/acme/app.git main

Every push sends a signed `push` hook to the panel. Send a pull request event
with `nodes.sh pr acme/app 1 opened my-branch [base]`
(action: opened, reopened, synchronize, closed).

A GitHub install with zero repos shows "not installed" until the first push.

An existing playground needs `dev.env` rewritten (`GITHUB_URL`, `GITHUB_CA`)
and the panel restarted:
`incus exec ctrl -- sh -c 'docker restart $(docker ps -q --filter label=stackr.role=panel)'`.

## S3

With the S3 mock on, add a backup destination with endpoint
`http://s3.stackr.test:9000`, bucket `stackr-backups` (made at boot), region
`us-east-1`. The credentials are in `/var/lib/playground/rustfs.env` on the VM.
The address is not private to stackr, so no LAN approval is asked.

## Shares

With the shares mock on, add org shares in Storage:

- NFS: source `203.0.113.1:/srv/shares/nfs`, options `nfsvers=4`. Docker mounts
  without a helper, and its default (NFSv3 over UDP) finds no listener on the
  server. A server that only speaks v4 needs `nfsvers=4` the same way.
- SMB: source `//203.0.113.1/playground`, user `stackr`. The password is an org
  param ref: set a secret param to the content of `/var/lib/playground/samba.pass`
  and use `${{ org.params.<collection>.<name> }}`.

A tile mounts them as `share:<slug>:/path`. The first deploy parks until a
server admin approves the stack's host access (Admin, Host access).

## Namecheap

With the Namecheap mock on, add the DNS connector as a server connector (Admin,
Connectors): `stackr.test` is the install's root domain, so an org connector
cannot manage it. Any API user and key work. Records land in
`/var/lib/playground/namecheap/dnsmasq.conf`; check them with
`dig @203.0.113.1 <name>.stackr.test`. `nodes.sh up` sets ctrl's public IP to
203.0.113.1 so tile records have a target.

## Notes

- `ext.latencyMs` is added one way, on wan0 egress (traffic leaving the VM
  toward ext), so a round trip costs it once.
