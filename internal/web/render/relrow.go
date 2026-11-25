package render

import (
	"cmp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/components"
)

// The words the releases window and the Releases screen share for a row.

// SourceWord is who or what cut a release, as a person reads it: names maps
// a user id to a name.
func SourceWord(by string, names map[string]string) string {
	id, ok := strings.CutPrefix(by, "user:")
	if ok || uuid.Validate(by) == nil { // a person (an id, never shown); an unnamed one is no email
		if id = cmp.Or(id, by); names[id] != "" {
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

// IsPerson says by names a panel user ("user:<id>" or a bare id).
func IsPerson(by string) bool { return strings.HasPrefix(by, "user:") || uuid.Validate(by) == nil }

// ViaWord is where a release came from: "GitHub" for a push, "image watch",
// else the panel.
func ViaWord(by string) string {
	switch by {
	case "push", "":
		return "GitHub"
	case "image-watch":
		return "image watch"
	}
	return "panel"
}

// WhoWord is the person line 2 names: a push's commit author, a panel
// user's display name; "" for an image watch or an unnamed user (never an id).
func WhoWord(by, authorName, authorLogin string, names map[string]string) string {
	switch by {
	case "push", "":
		return cmp.Or(authorName, authorLogin)
	case "image-watch":
		return ""
	}
	id := strings.TrimPrefix(by, "user:")
	if id != by || uuid.Validate(by) == nil {
		return names[id]
	}
	return ""
}

// AgoWord is d as "just now", "5m ago", "3h ago", "3d ago".
func AgoWord(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
}

// AvatarOf is who caused a release and where it came from (UI 10 RFC 2.9).
// A push shows its commit author: the picture by login, else the initials of
// the name; a login ending [bot] is a rounded square. A push cut before
// authors were kept shows the GitHub mark and no invented person. An image
// watch is an eye and no badge. Everything else is a panel action: the stackr
// mark alone, no person (the panel user is not kept on the release).
func AvatarOf(by, name, login string) components.Avatar {
	switch by {
	case "image-watch":
		return components.Avatar{Icon: "eye"}
	case "push", "":
		if name == "" && login == "" {
			return components.Avatar{Icon: "github", Badge: "github"}
		}
		a := components.Avatar{Initials: components.Initials(cmp.Or(name, login)), Badge: "github", Bot: strings.HasSuffix(login, "[bot]")}
		a.Login = login
		return a
	}
	return components.Avatar{Icon: "stackr"}
}

// WaitingPromote is the POST every list promote goes to.
const WaitingPromote = "/-/waiting/promote"

// PairOf is a release going into an env as a form value, "release:env".
func PairOf(releaseID, envID string) string { return releaseID + ":" + envID }

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

// ReleaseLineOf is a window row as the shared ReleaseLine shows it: the stack
// leads, a push shows its commit, every other kind says what cut it. who
// names users, envs maps an env id to its name and hue, jobHref is the
// address of a job's log, base the stack's address (the release modal is
// base/<env>/-/releases/<id>/dialog). Each caller adds its own scope.
func ReleaseLineOf(r service.WindowRow, stack, base string, who map[string]string, envs map[string]components.EnvRun, jobHref func(string) string) components.ReleaseLineView {
	out := components.ReleaseLineView{
		Stack: stack, Source: cmp.Or(SourceWord(r.By, who), "git push"), By: SourceWord(r.By, who),
		State: r.State, Tone: StateTone[r.State], Newest: r.Newest, At: r.At,
	}
	if out.By == "" { // a release cut before its source was kept
		out.By = "git push"
	}
	out.Avatar = AvatarOf(r.By, r.AuthorName, r.AuthorLogin)
	out.Who, out.Via = WhoWord(r.By, r.AuthorName, r.AuthorLogin, who), ViaWord(r.By)
	if !r.At.IsZero() {
		out.Age = AgoWord(time.Since(r.At))
	}
	if r.Number > 0 {
		out.Number = strconv.Itoa(r.Number)
	}
	if r.Lacks > 0 {
		out.Lacks = strconv.Itoa(r.Lacks)
	}
	if r.By == "image-watch" {
		out.Change = r.Cause
		out.NoVersion = r.NoVersion
	} else {
		out.Cause = r.Cause
	}
	if r.By == "push" || r.By == "" { // only a push shows a commit
		out.Commit, out.Message = ShortSHA(r.Commit), r.Message
	}
	switch r.Kind {
	case service.RowFailed:
		if r.FailEnv != "" { // a promote that failed, from the history: its release keeps its message, the cause gets its own line
			out.Err = strings.TrimRight(ErrTail(r.Err), ".")
		} else {
			out.Cause = "Push failed: " + strings.TrimRight(ErrTail(r.Err), ".")
		}
		out.Link = components.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
	case service.RowBuilding:
		out.Source, out.Commit, out.Message = "", "", ""
		out.Cause = "Building at " + ShortSHA(r.Commit)
		out.State, out.Tone = "building", "warn"
		if r.Queued {
			out.Cause = "Waiting to build at " + ShortSHA(r.Commit)
			out.State = "queued"
		}
		out.Link = components.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
	}
	for _, id := range r.Runs {
		out.Runs = append(out.Runs, envs[id])
	}
	if r.State == service.StateWaiting && len(r.WaitEnvs) > 0 {
		out.To = envs[r.WaitEnvs[0]].Name
	}
	if r.State == service.StateFailed && r.Kind == service.RowRelease {
		out.Link = components.RelLink{Label: "Job log", Href: jobHref(r.JobID)}
		out.State = "failed in " + r.FailEnv
		out.Err = strings.TrimRight(ErrTail(r.Err), ".")
	}
	if r.Kind == service.RowRelease && r.OpenEnv != "" {
		out.Open = base + "/" + r.OpenEnv + "/-/releases/" + r.ID + "/dialog"
	}
	return out
}

// ReleaseModalOf is the release modal's view of a service.ReleaseDetail;
// base is the env's address, write says the viewer may promote, rows that the
// modal was opened on the Waiting page. Promote is offered only when the
// service says it is clean and waiting.
func ReleaseModalOf(d service.ReleaseDetail, base string, write, rows bool, who map[string]string) components.ReleaseModalView {
	v := components.ReleaseModalView{
		Stack: d.Stack, Number: strconv.Itoa(d.Number), Env: d.Env,
		Source: cmp.Or(SourceWord(d.By, who), "git push"), Cause: d.Cause, By: d.By, At: d.At,
		Avatar: AvatarOf(d.By, d.AuthorName, d.AuthorLogin),
		Who:    WhoWord(d.By, d.AuthorName, d.AuthorLogin, who), Via: ViaWord(d.By),
		Commit: d.Commit, CommitURL: d.CommitURL, Message: d.Message,
		Problems: d.Problems, Blockers: d.Blockers, Impacts: d.Impacts, Removals: d.Removals, Gaps: d.Gaps,
		State: d.State, Tone: StateTone[d.State],
		Review: base + "/-/releases/" + d.ID,
	}
	if !d.At.IsZero() {
		v.Age = AgoWord(time.Since(d.At))
	}
	for _, c := range d.Changes {
		v.Changes = append(v.Changes, components.ReleaseChangeView{Label: c.Label, Old: c.Old, New: c.New})
	}
	if v.Commit != "" {
		v.Commit = ShortSHA(v.Commit)
	}
	if write && d.Promotable {
		v.Promote, v.Pair, v.Rows = WaitingPromote, PairOf(d.ID, d.EnvID), rows
	}
	return v
}

// WaitedWord is "waiting 3d": how long a release has waited, coarse.
func WaitedWord(d time.Duration) string {
	switch {
	case d < time.Hour:
		return "waiting under 1h"
	case d < 48*time.Hour:
		return "waiting " + strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return "waiting " + strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// WaitReleaseOf is a release row of the Waiting page: the shared line (from
// the env below, to this one, how long it has waited), and what the page adds.
// status is a refusal or failure of the last promote for this pair ("" = none).
func WaitReleaseOf(it service.WaitItem, who map[string]string, status string) *components.WaitReleaseView {
	r := it.Rel
	base := "/" + r.OrgSlug + "/" + r.StackSlug
	line := ReleaseLineOf(service.WindowRow{
		Kind: service.RowRelease, ID: r.ID, Number: r.Number, Commit: r.Commit, Message: r.Message, Cause: r.Cause,
		By: r.By, AuthorName: r.AuthorName, AuthorLogin: r.AuthorLogin, At: r.At, OpenEnv: r.EnvSlug,
	}, r.Stack, base, who, nil, nil)
	if r.Change != "" {
		line.Change = r.Change
	}
	line.NoVersion = r.NoVersion
	line.To, line.From = r.Env, r.From
	if !r.At.IsZero() {
		line.Waited = WaitedWord(time.Since(r.At))
	}
	if line.Open != "" {
		line.Open += "?on=waiting"
	}
	v := &components.WaitReleaseView{
		Line: line, Pair: PairOf(r.ID, r.EnvID), Problems: r.Problems,
		Review: base + "/" + r.EnvSlug + "/-/releases/" + r.ID, Promote: r.Promotable, PlanFailed: r.PlanFailed,
		Status: r.Status,
	}
	if status != "" {
		v.Status, v.Failed = status, true
	}
	return v
}
