# Plans

Only active plans retain implementation detail. Completed work is kept as a
short record at its original path because code and feature docs link to those
decisions.

## Open

| Plan | Status | Remaining work |
|---|---|---|
| [09 HAMR hosting](09-hamr-hosting.md) | discussion | Decide the platform contract for HAMR-built applications. |
| [14 waiting chain](14-waiting-chain.md) | shipped, two decisions open | Decide the unset-secret panel and org-scope reader behavior. |
| [16 install scripts](16-install-scripts.md) | partly exercised | Finish production-install coverage and fold upgrade work into the beta release. |
| [20 event-triggered functions](20-event-triggered-functions.md) | discussion | Decide the tile model, delivery guarantees and network ownership. |
| [33 workqueue](33-workqueue.md) | steps 1–3 shipped | Move backups, volume moves and cron runs onto the durable queue. |
| [45 lazy page loads](45-lazy-page-loads.md) | implemented, rig tested | Pages that block on docker, agents, GitHub or the registry: fragments or bounded calls with an unknown state, nine items. |
| [46 serverconfig gaps](46-serverconfig-gaps.md) | done | Org network shares, domain middlewares and rules, folder `files:`. |

The beta sequence is tracked in [Beta release](../beta-release.md). Product
gaps which are not approved plans live in [Notes](../notes.md).

## Completed and parked records

| Plan | Outcome |
|---|---|
| [01](01-config-overhaul.md) | Config-as-code overhaul shipped. |
| [03](03-image-watch-ci-gate.md)–[04](04-qa-image-watch-ci.md) | Image watcher and CI gate shipped and QA'd. |
| [05](05-stackr-plan-preview.md) | Client-side plan preview shipped. |
| [06](06-moved-directive.md) | Declarative `moved:` renames shipped. |
| [07](07-cli-ux-rewrite.md)–[08](08-cli-ux-matrices.md) | Cobra/Fang CLI rewrite shipped. |
| [10](10-onboarding-journey.md)–[11](11-onboarding-round2.md) | Onboarding wizard shipped and revised. |
| [12](12-plan-rows.md)–[13](13-proxy-probe.md) | Plan value rows and proxy verification shipped. |
| [15](15-theming.md) | Light theme shipped. |
| [17](17-docker-naming.md)–[19](19-runs-and-canvas-polish.md) | Naming, edit policy, runs and canvas work shipped. |
| [21](21-log-views.md)–[26](26-releases-page.md) | Logs, scheduling, promotion and release views shipped. |
| [27](27-auto-rollback.md) | Automatic rollback parked. |
| [28](28-empty-org-wizard.md)–[29](29-setup-gate-everything.md) | Empty-org wizard and setup gate shipped. |
| [30](30-docker-swarm.md)–[32](32-multi-node-ui.md) | Swarm, node agent and multi-node UI shipped. |
| [34](34-apply-and-review-fixes.md)–[39](39-review-fixes.md) | Apply safety, cluster boundary, builds, parity, restore and review fixes shipped. |
| [40](40-cloudflare.md) | Cloudflare connector parked. |
| [41](41-beta-gate.md)–[42](42-ignored-writes.md) | Beta gate and ignored-write audit shipped and rig-verified. |
| [43](43-panel-upgrade.md)–[44](44-agent-image-published.md) | Panel upgrade and node agents on a published install shipped and rig-verified. |
| [2026-09-11 review](2026-09-11-review-fixes.md) | Security and tenancy review fixes shipped. |
