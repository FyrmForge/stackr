package main

// The API review added these operations. The CLI has no verb for them yet;
// a verb for any of them is added to cmds.go and its line here goes.
func init() {
	for op, why := range map[string]string{
		"key.org-list":     "no CLI verb yet: an org owner lists the org's keys",
		"key.org-revoke":   "no CLI verb yet: an org owner revokes one of the org's keys",
		"admin.key-list":   "no CLI verb yet: an admin lists every key",
		"admin.key-revoke": "no CLI verb yet: an admin revokes any key",
		"health":           "liveness probe beside /api/v1, not a CLI concern",
		"metrics":          "Prometheus scrape beside /api/v1, not a CLI concern",
		"hook.connector":   "a git host's webhook delivery, signed, not a CLI concern",
		"join.script":      "the curl | sh join script, fetched by the shell, not a CLI concern",
		"join.installer":   "the installer binary the join script fetches, not a CLI concern",
	} {
		skipped[op] = why
	}
}
