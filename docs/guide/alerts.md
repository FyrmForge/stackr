# Alerts

stackr posts to one webhook when something needs you:

| Event | When |
|---|---|
| `node.offline` | A worker misses its health checks for about a minute. |
| `node.online` | That worker answers again. |
| `backup.failed` | A backup schedule starts failing. It fires again every 24 hours while it keeps failing, and resets on the next success. A backup skipped because its node is offline is not a failure. |
| `cert.expiring` | A node certificate, the controller certificate or the cluster root has under 14 days left, then again under 3. |
| `disk.low` | A node's Docker disk drops under 10% or 1 GB free. |
| `test` | You press Send test. |

Each event fires once per crossing. It fires again only after the condition clears and comes back, except the daily `backup.failed` reminder. A panel restart does not send again. Let's Encrypt certificates are not checked.

A message is at most 300 characters. A longer one loses its middle, so the end of an error stays.

## Set it up

1. Admin, Alerts. Paste the webhook URL and save.
2. Optional: set a signing secret.
3. Press Send test. The tab shows the last delivery and why it failed.

From the CLI:

```bash
printf '%s' "$SECRET" | stackr admin alerts set --url https://hooks.example.com/stackr --secret-stdin
stackr admin alerts test
stackr admin alerts show
stackr admin alerts clear
```

A webhook URL often carries a token in its path. The panel shows it as `https://host/…` everywhere: the Alerts tab, `alerts show` and `GET /admin/settings/alert_webhook_url`.

In `stackr-server.yml` the URL is a server param ref, never the URL itself:

```yaml
settings:
  alert_webhook_url: ${{ server.params.alerts.webhook_url }}
params:
  alerts:
    webhook_url:
      type: secret
```

The export writes it that way and stores the live URL in that param. On a new panel, set the param before you apply. The signing secret does not go in the file.

## The body

```json
{
  "event": "node.offline",
  "severity": "critical",
  "node": "worker-1",
  "subject": "worker-1",
  "message": "worker-1 is offline.",
  "at": "2026-10-18T09:00:00Z",
  "panel_url": "https://panel.example.com"
}
```

Severity is `info`, `warning` or `critical`.

With a secret set, each post carries `X-Stackr-Signature: sha256=<hex>`. That is the HMAC-SHA256 of the raw body, keyed with the secret. Check it before you trust the body.

A post times out after 10 seconds. A network error, a 5xx or a 429 is retried up to three more times. Any other answer is final.
