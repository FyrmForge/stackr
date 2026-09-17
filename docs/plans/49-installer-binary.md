# Plan: the installer as a Go binary with a form

Status: built and rig-verified 2026-09-17.

Working rules: discuss first, one point at a time, no code without a go, no
git writes, no edits to `*_templ.go` or `output.css`, terse UI copy.

## Why

`install.sh` asks its questions one plain line at a time. Arrow keys print
`^[[A`, there is no going back to an earlier answer, and bad answers are only
caught after enter. A real form needs a real program.

## Decisions (2026-09-17)

1. **Binary on GitHub Releases**, next to `install.sh`:
   `stackr-install-linux-amd64` and `stackr-install-linux-arm64`, plus
   `checksums.txt`. Not GitHub Packages: ghcr stores images, a plain binary
   there needs `oras`, and an installer run as a container has to reach out
   of it to edit `/etc/docker/daemon.json` and `/usr/local/bin`.
2. **All of the install moves into Go.** Form, swarm init and address pool,
   registry trust, pulls, service create, CLI wrapper, dry run.
   `install.sh` shrinks to: detect CPU, download, check the checksum, run.
   The checks share code with the panel (`proxy.ParseTrusted`).
3. **The form uses `charm.land/huh/v2`**, the charm stack the `stackr` CLI
   already uses. One question per screen with the answers so far listed above
   it (changed 2026-09-17, was one page of fields: huh prints a group's errors
   in a single footer under the last field, so on a full page the message for
   the first question landed fifteen lines away from it). Arrows and tab move
   between questions, questions that do not apply are skipped, toggles for
   yes/no, and a last summary screen: Install / Go back / Cancel.
4. **Docker through the `docker` command**, like the bash did, not the Docker
   Go library: same commands as the rig-tested script, a small binary, and a
   dry run that prints exactly what would run.
5. **A domain is required (changed 2026-09-17, was a "do you have a domain?"
   toggle).** Apps are reached by name through traefik and so is the panel, so
   an IP install has no names, no certificates and no way to the panel but a
   published port. That is a different product and needs its own thinking, not
   a branch in the form. The questions are root domain, panel host (default
   `stkr.<root>`), Cloudflare, proxy address, HTTPS; root and panel host
   refuse an IP and `localhost`.
6. **No HTTPS port when HTTPS is off.** The form hides the field, and the
   panel's Traefik service publishes only the HTTP port with `STACKR_TLS=off`
   (`infra/proxy/proxy.go` `EnsureTraefik`), so a busy 443 cannot stop it.
7. **DNS check, a warning you can skip.** With a domain and HTTPS on, before
   installing: look up the panel host and a made-up `<random>.<root>`. Either
   missing shows which record to add, with Continue anyway / Go back. No "does
   it point at this box" check: behind Cloudflare or a proxy it never does.
8. **Dry run: a busy port is a warning, not a block.** A dry run is usually on
   another machine than the install. A real run still blocks.
9. **Flags to install without the form**, for scripts and CI. Every question
   has a flag; `--yes` skips the form and the summary and fails on the first
   missing or bad value. Same checks as the form.

Unchanged from the bash: data dir `/var/lib/stackr` with `STACKR_DATA_DIR` to
move it, refuse a host that already runs the `stackr` service, the Let's
Encrypt email only with HTTPS on, trusted proxy seeding and `ROOT_DOMAIN` from
plan 48.

## Build

### A. `cmd/stackr-install` + `internal/installer`

- `answers.go`: the `Answers` struct and one validator per field (hostname,
  root/panel relation, IP, proxy list via `proxy.ParseTrusted`, email, port,
  data dir). Ported from the bash validators, same rules.
- `form.go`: the huh form, hidden fields, summary screen.
- `flags.go`: cobra flags mapping onto `Answers`, `--yes`, `--dry-run`,
  `--version` (default: the installer's own version, so the binary installs
  the release it came from; no GitHub API call).
- `steps.go`: the install as a list of steps, each a `docker ...` or file
  write. `Run` executes them; `--dry-run` prints them one flag per line.
  Ported: swarm init + pool pick (`cidr_overlaps`, `used_subnets`,
  `advertise_addr`), registry trust (merge `daemon.json`, restart docker, wait),
  overlay network, pulls, relay retag, service create, CLI wrapper, done text
  with DNS records.
- `dns.go`: the plan-7 check.
- Tests: validators (the cases in `scripts/install_test.sh`), pool overlap,
  the step list for a domain/HTTPS and a domain/no-HTTPS answer set,
  `--yes` with a missing value.

### B. `scripts/install.sh`

Replaced by the small downloader: root check, `uname -m` to amd64/arm64,
`curl` the binary and `checksums.txt` for the same version, `sha256sum -c`,
run it with `</dev/tty` and all arguments passed through. Still one `{ }`
block. `scripts/install_test.sh` is deleted, its cases live in Go.

### C. CI (`.github/workflows/release.yml`)

After the release job: build both architectures with `CGO_ENABLED=0` and the
version in `-ldflags`, write `checksums.txt`, `gh release upload` all three
plus `install.sh`.

### D. Panel

- `infra/proxy/proxy.go`: no HTTPS port published with TLS off.
- `Makefile`: `installer` target for a local build.

### E. Docs

`docs/host-setup.md` install section: the form, `--dry-run`, the flags.

## Verify

- Local: `stackr-install --dry-run` with the form, arrows, go back, summary;
  a `--yes` run with flags.
- Rig: wipe, run the binary built locally (images local, pulls skipped the way
  the plan 48 test did), domain + HTTPS. Panel up, seeding as in plan 48.
- After the next release: the real `curl | sudo bash` path downloads and
  checks the binary.

## How it was built (differs from above)

- `proxy.ParseTrusted` moved to `internal/netaddr`. The installer cannot
  import `infra/proxy`: that package pulls in the docker client, the store and
  sqlite, all of it into a binary people curl. The panel calls the new package
  from `cmd/stackrd/seed.go` and `handlers/web/handler/settings/proxy.go`.
- Flags use the standard library `flag`, not cobra and fang: fang owns
  `--version`, which here names the release to install.
- The DNS check is not its own screen. Its warnings are printed on the summary
  above Install / Go back / Cancel, so there is one confirmation, not two.
- Nothing on the box is touched until Install is picked. The bash ran the
  swarm init and the docker restart before the questions.
- The installer skips a pull for an image already on the box (release tags do
  not change). That is what let the rig run it against locally built images.
- `--dry-run` prints a busy-port warning after the form instead of blocking,
  and `portBusy` tries a connect first: a dry run is not root, so it cannot
  bind a low port either way.
- The arrow keys are bound in the keymap (`up`/`down` as back/next), and the
  summary's filter key is off.
- The form asks for a fixed height (`formHeight`, capped by the terminal).
  Left alone huh measures the groups before the recap notes have rendered,
  sizes every group to the shortest, and the first answers then scroll out of
  the recap.
- `TRAEFIK_HTTPS_PORT` is left out of the service env with HTTPS off, rather
  than passed empty.

## Verified

Local: the form driven in tmux (arrows between fields, an inline error for a
panel host outside the root, Go back
keeping every answer, Ctrl+C cancelling), `--yes` for all three answer sets,
and `install.sh` against a fake release served locally (latest, `--version
X.Y.Z`, `--version=X.Y.Z`, a missing release, and a tampered binary refused on
the checksum).

Rig (`stackr-test.vulpe.dev`), wiped before each: the form with a domain and
HTTPS, an install without a domain (that path has since been dropped, see
decision 5), and a `--yes` install. All three came up; the panel answered on
its hostname over HTTPS,
`/etc/docker/daemon.json` gained the registry without losing the key already
in it, `install_seeded`, `trusted_proxies=192.168.1.100/32` and the instance
domain were seeded, and with HTTPS off traefik published only port 80.

One glitch, not fixed: over `ssh -tt` the first key pressed on a newly started
form can be dropped (bubbletea reading the terminal's capability replies). A
second press works.
