package render

import (
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

// The words the releases window and the Releases screen share for a row.

// SourceWord is who or what cut a release, as a person reads it: names maps
// a user id to a name.
func SourceWord(by string, names map[string]string) string {
	if id, ok := strings.CutPrefix(by, "user:"); ok { // a person; an unnamed one is no email
		if names[id] != "" {
			return names[id]
		}
		return "a member"
	}
	switch by {
	case "push":
		return "git push"
	case "image-watch":
		return "image watch"
	}
	return by
}

// StateTone is a row state's chip tone; the rest are neutral.
var StateTone = map[string]string{
	service.StateWaiting:   "accent",
	service.StateDeploying: "warn",
	service.StateRunning:   "success",
	service.StateFailed:    "danger",
}

// ShortSHA is the seven characters a commit shows as.
func ShortSHA(sha string) string { return sha[:min(7, len(sha))] }

// ErrTail is the end of an error, where the cause is.
func ErrTail(err string) string {
	s, _ := components.ErrTail(err, 80)
	return s
}

// RelRowOf is the part of a row the releases window and the Releases screen
// share: the columns, the words of a failure or a build, the envs it runs in
// and its jobs. who names users, envs maps an env id to its name and hue,
// jobHref is the address of a job's log. Each caller adds its own review
// links and, on the screen, its scope.
func RelRowOf(r service.WindowRow, stack string, who map[string]string, envs map[string]components.EnvRun, jobHref func(string) string) components.RelRow {
	out := components.RelRow{
		Kind: r.Kind, ID: r.ID, Commit: ShortSHA(r.Commit), Message: r.Message, State: r.State, Tone: StateTone[r.State],
		Newest: r.Newest, At: r.At, CommitURL: r.CommitURL, Stack: stack, Queued: r.Queued, Fixed: r.Fixed,
		By: SourceWord(r.By, who),
	}
	if out.By == "" { // a release cut before its source was kept
		out.By = "git push"
	}
	if r.Number > 0 {
		out.Number = strconv.Itoa(r.Number)
	}
	if r.Lacks > 0 {
		out.Lacks = strconv.Itoa(r.Lacks)
	}
	if out.Message == "" && r.Kind == service.RowRelease {
		out.Message = strings.ToUpper(out.By[:1]) + out.By[1:]
	}
	switch r.Kind {
	case service.RowFailed:
		out.Message = "Push failed: " + strings.TrimRight(ErrTail(r.Err), ".")
		if r.FailEnv != "" { // a promote that failed, from the history
			out.Message = "Release " + out.Number + " failed in " + r.FailEnv + ": " + strings.TrimRight(ErrTail(r.Err), ".")
		}
		out.Link = components.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
	case service.RowBuilding:
		out.Link = components.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
	}
	if r.Kind == service.RowRelease && r.FailEnvID != "" {
		out.ID += ":" + r.FailEnvID // one row per env it failed in, each its own open state
	}
	for _, id := range r.Runs {
		out.Envs = append(out.Envs, envs[id])
	}
	if r.State == service.StateFailed && r.Kind == service.RowRelease {
		out.Link = components.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
		out.State = "failed in " + r.FailEnv
	}
	for _, j := range r.Jobs {
		t := j.Kind + ", " + j.State
		if j.Took > 0 {
			t += " in " + j.Took.Round(1e9).String()
		}
		out.Jobs = append(out.Jobs, components.RelJob{Text: t, Href: jobHref(j.ID)})
	}
	return out
}
