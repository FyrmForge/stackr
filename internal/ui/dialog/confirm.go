// Package dialog is the env level's create form and its confirm wrappers.
// A confirm's answer swaps Target (outerHTML); the drawers pass their own
// root, never #drawer-body.
package dialog

import c "github.com/FyrmForge/stackr/internal/ui/components"

func Restart(tile, action, target string) c.ConfirmView {
	return c.ConfirmView{Button: "Restart", Title: "Restart " + tile + "?",
		Warning: "Its replicas restart one by one; a single replica drops requests while it starts.",
		Action:  action, Target: target}
}

func Stop(tile, action, target string) c.ConfirmView {
	return c.ConfirmView{Button: "Stop", Title: "Stop " + tile + "?",
		Warning: "It serves nothing until started again.", Action: action, Target: target}
}

func Delete(tile, action, target string) c.ConfirmView {
	return c.ConfirmView{Button: "Delete", Title: "Delete " + tile + "?",
		Warning: "Its containers, domains and run history go.",
		Kept:    []string{"its volumes, orphaned for the retention window", "backups already in a destination"},
		Word:    tile, Action: action, Target: target}
}

// Rollback puts the env back on an older release.
func Rollback(env, release, action, target string) c.ConfirmView {
	return c.ConfirmView{Button: "Roll back", Title: "Roll " + env + " back to " + release + "?",
		Warning: "Every tile redeploys on what that release pins.",
		Kept:    []string{"volumes and their data", "params and secrets as they are now"},
		Word:    env, Action: action, Target: target}
}
