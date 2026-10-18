# Alerts

stackr posts to one webhook when something needs you:

| Event | When |
|---|---|
| `node.offline` | A worker misses its health checks for about a minute. |
| `node.online` | That worker answers again. |
| `backup.failed` | A scheduled volume or panel backup fails. A backup skipped because its node is offline is not a failure. |
| `cert.expiring` | A node certificate, the controller certificate or the cluster root has under 14 days left, then again under 3. |
| `disk.low` | A node's Docker disk drops under 10% or 1 GB free. |
| `test` | You press Send test. |

Each event fires once per crossing. It fires again only after the condition clears and comes back. Let's Encrypt certificates are not checked.

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

The URL can also go in `stackr-server.yml` as `settings.alert_webhook_url`. The secret cannot.

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
