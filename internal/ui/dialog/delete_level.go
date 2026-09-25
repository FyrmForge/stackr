package dialog

import c "github.com/FyrmForge/stackr/internal/ui/components"

// Delete confirms for the home, org and stack level cards. The answer is a
// redirect to the canvas, or the drawer again with the refusal (target).

func DeleteOrg(name, action, target string) c.ConfirmView {
	return c.ConfirmView{
		Button:  "Delete org",
		Title:   "Delete " + name + "?",
		Warning: "Its members, keys, connectors and params go. Refused while it has stacks.",
		Kept:    []string{"backups already in a destination"},
		Word:    name,
		Action:  action,
		Target:  target,
	}
}

func DeleteStack(name, action, target string) c.ConfirmView {
	return c.ConfirmView{
		Button:  "Delete stack",
		Title:   "Delete " + name + "?",
		Warning: "Its releases and params go. Refused while it has envs.",
		Kept:    []string{"backups already in a destination"},
		Word:    name,
		Action:  action,
		Target:  target,
	}
}

func DeleteEnv(name, action, target string) c.ConfirmView {
	return c.ConfirmView{
		Button:  "Delete env",
		Title:   "Delete " + name + "?",
		Warning: "Its params go and the ladder closes the gap. Refused while it has tiles.",
		Kept:    []string{"the stack's releases"},
		Word:    name,
		Action:  action,
		Target:  target,
	}
}

func DeleteConnector(name, action, target string) c.ConfirmView {
	return c.ConfirmView{
		Button:  "Delete connector",
		Title:   "Delete " + name + "?",
		Warning: "stackr forgets the app; uninstall it on GitHub too.",
		Action:  action,
		Target:  target,
	}
}
