package dialog

import (
	"encoding/json"

	c "github.com/FyrmForge/stackr/internal/ui/components"
)

// Delete confirms for the home, org and stack level cards. The answer is a
// redirect to the canvas, or the drawer again with the refusal (target).

func DeleteOrg(name, action, target string) c.ConfirmView {
	return c.ConfirmView{
		Button:  "Delete org",
		Title:   "Delete " + name + "?",
		Warning: "Its members, keys, connectors and params go. Refused while it has stacks.",
		Kept:    []string{"backups already in a destination", "its volumes, for the orphan retention; a last backup is taken before they go"},
		Word:    name,
		Action:  action,
		Target:  target,
	}
}

// DeleteStack takes the stack and everything in it; slug is the word typed.
func DeleteStack(name, slug, action, target string) c.ConfirmView {
	vals, _ := json.Marshal(map[string]string{"confirm": slug})
	return c.ConfirmView{
		Button:  "Delete stack",
		Title:   "Delete " + name + "?",
		Warning: "Its tiles, envs, releases and params go.",
		Kept:    []string{"backups already in a destination", "its volumes, for the orphan retention; a last backup is taken before they go"},
		Word:    slug,
		Action:  action,
		Target:  target,
		Vals:    string(vals),
	}
}

func DeleteEnv(name, action, target string) c.ConfirmView {
	return c.ConfirmView{
		Button:  "Delete env",
		Title:   "Delete " + name + "?",
		Warning: "Its variables go and the ladder closes the gap. Refused while it has tiles.",
		Kept:    []string{"the stack's releases", "its volumes, for the orphan retention; a last backup is taken before they go"},
		Word:    name,
		Action:  action,
		Target:  target,
	}
}

// PurgeLogs asks before an env's kept log lines go.
func PurgeLogs(env, action, target string) c.ConfirmView {
	return c.ConfirmView{
		Button:  "Delete this env's logs",
		Title:   "Delete the logs of " + env + "?",
		Warning: "Every kept log line of this environment goes.",
		Kept:    []string{"live logs of running tiles", "logs of other environments"},
		Action:  action,
		Target:  target,
	}
}

// DeleteConnector is a connector's delete; label names its provider.
func DeleteConnector(name, label, action, target string) c.ConfirmView {
	return c.ConfirmView{
		Button:  "Delete connector",
		Title:   "Delete " + name + "?",
		Warning: "stackr forgets it; uninstall it on " + label + " too.",
		Action:  action,
		Target:  target,
	}
}
