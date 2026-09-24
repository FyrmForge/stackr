# Webhook receiver

Source: `handlers/web/handler/prhook/` — handler.go, watch.go and their tests (route: `handlers/web/server.go:782`)
Commit: c2423f0
Taken: signature verification, event dispatch, payload field sets, response codes
Cut: every call that enqueues a deploy, applies config, creates or tears down an environment
Cuts belong to: `flow/promote` (push → release) or `leaf/environment` (PR env)

Target: verify + parse land in `service/internal/githubapp`; the route itself is
step 4's API. The receiver's whole job is: check the HMAC, decode the payload
into a typed event, hand it over. Nothing below the decode belongs to it.

## Route and secret

`POST /hooks/connectors/:id` — one endpoint per connector row, registered with
the logging middleware only (no auth middleware: the signature *is* the auth).
The secret is the connector's own webhook secret, read out of its stored config
blob (`githubapp.ParseConfig(cn.Config).WebhookSecret`). No secret stored → the
connector is not connected → 404, before the body is read.

There is a second, older receiver on the same file, `POST /hooks/github/:stack`,
whose secret comes from the stack's PR config row instead. Same
`validSignature`, same `prPayload`, `pull_request` only. The connector route is
the one that survives; the stack route's only unique behaviour is that it
refuses with 404 when PR environments are disabled.

## Signature verification

```go
// validSignature checks GitHub's X-Hub-Signature-256 over the raw body.
// An empty secret never validates — an unconfigured connector must not be
// an open endpoint.
func validSignature(secret, header string, body []byte) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.TrimPrefix(header, "sha256=")))
}
```

- Header: `X-Hub-Signature-256`, value `sha256=<hex>`. The older `sha1` header is
  not read at all.
- Body: the raw bytes, read once through `io.ReadAll(io.LimitReader(body, 1<<20))`
  — 1 MiB cap — and reused for both the MAC and the JSON decode. Never re-read,
  never decoded first.
- `hmac.Equal`, not `==`. Constant time.
- Event type: `X-GitHub-Event`, read *after* the signature check.

Test vector, kept verbatim from `handler_test.go` (`echo -n '{"x":1}' | openssl
dgst -sha256 -hmac s3cret`):

```go
body := []byte(`{"x":1}`)
sig := "sha256=75e31067a7b58ac9207ca9b950a2104dbc31159e3dc2f2881ffd48f614e8786c"
assert.True(t, validSignature("s3cret", sig, body))
assert.False(t, validSignature("s3cret", "sha256=deadbeef", body))
assert.False(t, validSignature("", sig, body), "empty secret must never validate")
```

## Payloads

```go
type prPayload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		CloneURL string `json:"clone_url"`
		SSHURL   string `json:"ssh_url"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type pushPayload struct {
	Ref     string `json:"ref"`     // refs/heads/<branch>
	After   string `json:"after"`   // head commit SHA
	Deleted bool   `json:"deleted"` // branch/tag deletion push
	Commits []struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
	Repository struct {
		CloneURL string `json:"clone_url"`
		SSHURL   string `json:"ssh_url"`
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// changedFiles flattens the push's touched paths. Empty when the payload
// carries no commit list (force pushes, very large pushes) — callers must
// treat that as "anything could have changed", never as "nothing changed".
func (p *pushPayload) changedFiles() []string {
	var out []string
	for _, c := range p.Commits {
		out = append(out, c.Added...)
		out = append(out, c.Removed...)
		out = append(out, c.Modified...)
	}
	return out
}
```

The PR head **SHA is not decoded** — only `head.ref` (the branch name). A PR
deploy builds the branch tip, not the exact commit GitHub announced. Push is
the only event that carries a SHA (`after`), and that SHA is what the CI gate
and a promote key on.

If the rewrite wants PR builds pinned to a commit, that is a new field on
`prPayload` — check GitHub's current `pull_request` payload for the spelling,
this struct never decoded one.

## Dispatch

The receiver verifies and decodes; the route (step 4's API) does the lookup and
writes the response; the flow does the work.

```go
// Event is what a verified delivery amounts to. Exactly one of PR or Push is
// set; both nil means an event nobody handles, which is not an error.
type Event struct {
	PR   *prPayload
	Push *pushPayload
}

// ErrBadPayload → 400, ErrBadSignature → 401. Anything else the caller maps.
var (
	ErrBadSignature = errors.New("bad signature")
	ErrBadPayload   = errors.New("bad payload")
)

// Receive verifies the delivery against the connector's webhook secret and
// decodes it. Body is the raw bytes, read once by the caller under a 1 MiB
// limit — the same bytes feed the MAC and the decode.
//
// extract: dropped the connector row lookup, belongs in step 4's API — the
// receiver takes the secret as a fact, and an empty secret (connector not
// connected) is that layer's 404, checked before the body is read.
func Receive(event, signature string, body []byte, secret string) (Event, error) {
	if !validSignature(secret, signature, body) {
		return Event{}, ErrBadSignature
	}
	switch event {
	case "pull_request":
		var p prPayload
		if err := json.Unmarshal(body, &p); err != nil || p.Number == 0 {
			return Event{}, ErrBadPayload
		}
		return Event{PR: &p}, nil
	case "push":
		var p pushPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return Event{}, ErrBadPayload
		}
		return Event{Push: &p}, nil
	default:
		return Event{}, nil // 200 {"status":"ignored"}
	}
}

// extract: dropped the pull_request fan-out (list the org's stacks, skip other
// orgs, refresh the config plan comment, check PR envs are enabled, check a
// tile in the stack tracks this repo, then open/sync/close), belongs in
// leaf/environment. Rules it carried: one stack's failure must not starve the
// rest of the org — count it, log it, keep going — and the counts become the
// response, 500 when any failed.
// extract: dropped planConfigs (re-plan every config-managed stack bound to
// this repo+branch, org config first, auto-apply onto the work queue; org
// applies are always human-approved, never webhook-driven) and autoDeploy
// (redeploy every git service/cron tile in the org's *default* environments
// tracking this repo+branch — envs above the default take images by promote,
// never from a push; PR-env tiles are covered by "synchronize" instead),
// belongs in flow/promote.
```

`pull_request` actions, from `dispatch` — the receiver only needs to know these
four strings map to three outcomes:

| action | outcome |
|---|---|
| `opened`, `reopened` | create the `pr-<number>` environment; if it already exists, fall through to sync |
| `synchronize` | redeploy the PR env's git tiles |
| `closed` | tear the PR env down |
| anything else | no-op, still 200 |

The env slug is `fmt.Sprintf("pr-%d", p.Number)` and its display name is
`"PR " + number`. `p.PullRequest.Base.Ref` is read for two things: the
`against:` filter (only build PR envs for PRs targeting listed base branches)
and the plan-preview comment ("what would merging into this base change").

**No `installation` branch exists.** Grepping `X-GitHub-Event` across
`internal/stackrd/` finds it only in this file, and this switch has no
`installation` case — an installation, installation_repositories or
check_suite delivery lands on `default` and is answered 200 `{"status":
"ignored"}`. Installation state is maintained by the App's own OAuth/setup
callback in `infra/githubapp`, never by a webhook. If the rewrite wants
install/uninstall to be reflected without a person revisiting the connector
page, that is a new branch, not a port.

## Response codes

| condition | code | body |
|---|---|---|
| connector unknown | whatever `stackrmw.HTTP` maps the store error to (404 for not-found) | error |
| connector has no webhook secret | 404 | `connector not connected` |
| body unreadable | 500 | error |
| signature missing, malformed, or wrong | 401 | `bad signature` |
| `pull_request` with unparseable body or `number == 0` | 400 | `bad payload` |
| `pull_request`, every target stack handled | 200 | `{"handled":n,"failed":0}` |
| `pull_request`, one or more stacks errored | 500 | `{"handled":n,"failed":m}` |
| `push` with unparseable body | 400 | `bad payload` |
| `push` | 200 | `{"deployed":n,"planned":m}` |
| any other event | 200 | `{"status":"ignored"}` |

The 500 on `failed > 0` is deliberate: it is what puts a red delivery in
GitHub's own delivery log, which is the only place an operator sees that a
webhook did not do its job. Keep it, and keep it *after* the loop — the
partial work still counts.

## Idempotency / replay

**There is none.** No `X-GitHub-Delivery` id is read, stored or de-duplicated;
no nonce, no timestamp window, no seen-deliveries table. A replayed delivery
re-runs the whole dispatch. Three things make that survivable today, and the
rewrite should keep at least the first two rather than re-derive them:

- an `opened` for an env that exists degrades to a sync, not a duplicate create;
- a queued deploy of the same tile supersedes the older one, so a double
  delivery costs a superseded row, not two builds;
- teardown of an already-gone env is a no-op.

The 1 MiB `io.LimitReader` is a size guard, not a replay guard.

## Pure helpers, kept as rules

These are small, tested and side-effect free, but they answer "which tiles does
this delivery touch", which is the flow's question, not the receiver's.

```go
// extract: dropped sameRepo/normalizeRepo, belongs in flow/promote. Rule: a
// tile's git URL matches the payload when the normalized forms are equal —
// lowercase, trim ".git", strip https:// http:// ssh:// git@, replace the
// first ":" with "/" (git@host:org/repo -> host/org/repo), trim "/". Compare
// against clone_url, ssh_url and full_name. Tested: the four spellings of
// github.com/acme/shop all normalize alike.

// extract: dropped watchMatch, belongs in flow/promote. Rule: watch is one
// regex per line, "!" prefix = ignore. A file counts when it matches no ignore
// rule and (there are no positive rules, or it matches one); the push deploys
// when any changed file counts. Empty watch config deploys. Empty changed list
// (force push, oversized push) deploys. Invalid regex lines are skipped, never
// fatal.

// extract: dropped configOnlyPush, belongs in flow/promote. Rule: a push whose
// every changed file is the stack's config path (default when unset) re-plans
// but must not rebuild — nothing in the image changed. An empty changed list is
// not config-only.
```

## Notes for the builder

- **Verify before you look at anything else.** The secret lookup is the only
  work that precedes the MAC; the event type is read after it. Do not decode
  JSON to route before verifying — the body is attacker-controlled until the
  HMAC says otherwise.
- **One body read.** MAC and decode share the same `[]byte`. A re-read (or an
  `echo.Bind`) verifies one set of bytes and acts on another.
- **Keep the empty-secret guard inside `validSignature`.** It is the difference
  between a half-configured connector being inert and being a public deploy
  trigger; a caller-side check is one refactor away from being forgotten.
- **Branch, not SHA, on `pull_request`.** If the rewrite pins PR builds to a
  commit, read `pull_request.head.sha` — do not assume the branch tip still is
  that commit by the time the build runs.
- **`push` is two independent jobs on one delivery** (re-plan config, rebuild
  tiles). They shared a request and a "already queued" set in the old code, and
  that set was already abandoned because the apply became asynchronous. In the
  rewrite they are two messages; supersede-by-tile is what keeps one push from
  being two builds.
- **Ten seconds.** GitHub hangs up on a delivery at roughly ten seconds, and a
  client disconnect cancels the request context — anything slower than a decode
  must go on the queue with a context that is not the request's. This is written
  in the old code as the reason auto-apply moved onto the work queue.

Size: source 838 lines, extract 294 lines.
