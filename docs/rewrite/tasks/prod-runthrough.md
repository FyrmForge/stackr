# Prod run-through (v0.7.0 -> v0.7.1)

Status: agreed with darthvader 2026-10-14. Runs after the multi-node finish rig proof is green.
darthvader approved exactly two master pushes for it: v0.7.0, then a fix release v0.7.1.

## Before the push

1. Run the CI checks locally on the final multi-node: the generated-files diff, `make test`, golangci-lint 2.14, templ lint.
2. Security sweep: semgrep, gitleaks, trivy on the built image, govulncheck, then a review of the findings. Fix the real ones.
3. Update the install and multi-node docs so they are simple to follow. The run-through follows them as written.
4. Make sure dev-only surfaces (component gallery, fake host, debug routes) are off in a prod build.

## Run-through

After the push, check that the v0.7.0 release page has the installer, and that `ghcr.io/fyrmforge/stackr:0.7.0` pulls.

1. Wipe the rig. Install v0.7.0 on 109 from the public installer and the ghcr image only.
2. Run the onboarding flow end to end.
3. Single node first: deploy, domains, backups, with no workers.
4. Real Let's Encrypt certs for the panel and app domains (and the wildcard, if a DNS provider is set).
5. Join ct110, vm106, h4 and h3 with the commands the panel prints (they pull ghcr).
6. Git build: install the GitHub App, push to a stackr-test repo, build on a builder worker, deploy.
7. A private ghcr app image through the org credential, with ghcr blocked on the node.
8. Backups to S3 on darthvader's laptop. The `stackr-rustfs` container serves `http://192.168.1.203:9100`, bucket `stackr-backups`, credentials in the scratchpad `rustfs/creds.env`; Claude runs it. Back up, restore, then restore the panel onto a fresh controller with the workers attached.
9. Roles: owner vs member vs admin.
10. Release v0.7.1 (a `fix:`), click Update, and check that the panel and every worker upgrade from ghcr.
