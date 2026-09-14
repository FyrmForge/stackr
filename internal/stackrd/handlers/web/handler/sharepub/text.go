package sharepub

import (
	"fmt"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// The wording below is all the context an outsider gets: they never see the
// org, stack or environment a link belongs to, only what they're being asked
// for. The label is the operator's own note, so it is the one place any
// identifying detail can appear.

func linkTitle(l *repo.SecretLink) string {
	if l.Label != "" {
		return l.Label
	}
	if l.Kind == repo.LinkShare {
		return "Shared with you"
	}
	return "Send credentials securely"
}

func linkBlurb(l *repo.SecretLink) string {
	if l.Kind == repo.LinkShare {
		return "Open this once to read the values. Copy them somewhere safe before you close the page."
	}
	return "Paste each value below. They go straight into encrypted storage. Nobody sees them in transit, and this page can only be used once."
}

func linkAction(l *repo.SecretLink) string {
	if l.Kind == repo.LinkShare {
		return "Reveal the values"
	}
	return "Send securely"
}

func revealBlurb(l *repo.SecretLink) string {
	if l.WindowMinutes > 0 {
		return fmt.Sprintf("Copy these now. The link stops working %d minutes after it was first opened.", l.WindowMinutes)
	}
	return "Copy these now. This link has been used up and won't open again."
}
