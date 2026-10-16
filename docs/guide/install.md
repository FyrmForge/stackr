# Install a panel

The panel is the controller: the web UI, the API and the first node. One box runs it.

## Before you start

- A Linux box, and you are root on it.
- Docker Engine 26.0 or newer. If Docker is missing, the installer offers to install it. An installed Docker is never upgraded.
- A domain you control, for example `apps.example.com`.
- Free ports TCP 80 and 443, and UDP 51871.
- The full list of host requirements is in the [README](../../README.md#system-requirements). The installer checks all of it first and names what is missing.

## Install

1. Download the installer from the latest GitHub release.

   ```bash
   curl -fL -o stackr-install \
     https://github.com/FyrmForge/stackr/releases/latest/download/stackr-install-linux-amd64
   chmod +x stackr-install
   ```

2. Run it as root. In a terminal it asks for the domain, an ACME contact email and the admin login.

   ```bash
   sudo ./stackr-install
   ```

   To skip the questions, pass flags. The password is never a flag; it comes from the environment.

   ```bash
   sudo STACKR_ADMIN_PASSWORD='replace-with-a-strong-password' ./stackr-install \
     --domain apps.example.com \
     --email you@example.com \
     --admin-email you@example.com
   ```

3. Save the recovery passphrase the installer prints. It is shown once. It opens the panel's own backups, and a restore on another box needs it.

4. With HTTPS (the default), add the two DNS records the installer prints, both pointing at this box's public IP.

   ```
   stkr.apps.example.com   A   <public IP>
   *.apps.example.com      A   <public IP>
   ```

   The second one is needed. Every stack gets a name under the domain, and each name must resolve before a certificate can be issued. With `--https=false` the installer prints no records; point the same names at the box yourself.

5. The installer logs the CLI on this box in as the admin, server admin commands included. It stores the key in `/var/lib/stackr/cli.json` and never prints it. `stackr` reaches the panel at its public URL, so it works once DNS and the certificate do. See [Log the CLI in](nodes.md#log-the-cli-in).

Flags you may want:

| Flag | What it does |
| --- | --- |
| `--domain` | Root domain. Required. |
| `--panel-host` | The panel's own name. Default `stkr.<domain>`. |
| `--email` | ACME contact address. Required with HTTPS. |
| `--admin-email` | The first admin. |
| `--https=false` | Serve plain HTTP. The panel is then on `http://`. |
| `--proxy` | CDN or load balancer ranges (for example Cloudflare's) whose X-Forwarded-For is trusted. |
| `--dns-token` | Cloudflare API token, for wildcard certificates through DNS-01. |
| `--http-port`, `--https-port` | Host ports. Defaults 80 and 443. |
| `--wg-port` | UDP port of the mesh. Default 51871. |
| `--version` | Install another release than the installer's own. |
| `--dry-run` | Print what would change and change nothing. |
| `--yes` | Install Docker without asking when it is missing. |

Running the installer again converges on the saved answers. A flag that disagrees with them is refused.

## Check it

1. Open `https://stkr.apps.example.com` (your own domain; `http://` with `--https=false`).
2. Log in with the admin email and password.

Until DNS is live there is no browser route in, because the panel publishes no port. The installer also put a `stackr` command on the box. It works without DNS:

```bash
stackr --help
```

## First login and onboarding

1. With no organization yet, the panel sends you to `/setup`. Pick a path.
   - **Fill it in here:** name the org, connect GitHub, add a domain, invite people.
   - **Describe it in a config file:** one file in a GitHub repo declares the org. You connect GitHub, pick the repo, review the plan and approve it.
2. Connect GitHub. The panel takes you to GitHub to create its app.
3. Add a domain (fill-in path). A config-file org gets its domains from the file.
4. Invite people on the Team step. You can do it later from the org's Members tab.
5. Press the finish button. The org is ready.

Next: [add a worker node](nodes.md), [back up](backups.md), [upgrade](upgrade.md).
