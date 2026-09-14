---
name: hamr-pr-publish
description: Create or update a GitHub PR for the current branch with a full structured body and fresh gist-hosted screenshots driven from the live HAMR dev server.
---

# PR publish

Create a new PR — or refresh an existing one — for the current branch, with
a structured body and real screenshots.

## Ground rules

- **Never commit or push the branch unless the user explicitly asked.** If
  the working tree is dirty and the user didn't say to commit, stop and ask.
- No AI attribution anywhere: commits, PR body, gist commits.
- Screenshots come from the app driven live — never mock, never crop out
  bugs. The hamr dev widget (bottom-left) in shots is acceptable.
- `hamr dev` must be running; server-side actions go through hamr MCP
  (`dev.info` for the proxy URL, `make.run` for make targets — your client
  may surface these underscored, e.g. `mcp__hamr__dev_info`).

## 1. Content from the diff

Analyse `git log`/`git diff` vs the default branch (plus anything the user
names). Derive:
- the feature story (one intro paragraph: what the user-facing outcome is)
- "What's included" bullets — features, not files
- hardening/fix bullets — review + QA work, each with *what it prevents*
- DB migrations touched
- what testing actually happened (gates + manual/QA loops; link any QA doc
  under `docs/qa/`)
- reviewer notes: out-of-scope items, known follow-ups (link the docs)

## 2. Screenshots

- **Reseed first** for clean, realistic data if the project has a reseed
  make target (check `dev.info` / the Makefile; run it via `make.run`).
  Reseeding is destructive and dev-only — confirm nothing in the dev DB is
  precious.
- If a state the PR touches isn't in the seed (e.g. a fresh *paid* order),
  create it live: drive the flow, complete payment with hamr's mock Stripe
  (`stripe.complete`).
- Drive with Playwright MCP at **1440×900**, one shot per key state, saved to
  `.playwright-mcp/pr/` with stable numbered names (`01-list.png`, `02-…`).
  Keep names stable across refreshes — the gist raw URLs embed the filename
  and track the latest revision, so same name = PR images update with **no
  body edit needed**.
- **Look at every shot before publishing** (Read the PNG) — no broken
  layouts, no QA junk data, no placeholder/1×1 images in frame.

### Flow GIF (lead image)

Playwright MCP can't record video, but a stepped GIF of the whole flow
works better than any single shot — it goes **first in the PR body**,
right after the title, before the intro paragraph.

1. Drive the full flow live (all roles involved, end to end), capturing one
   PNG per step into `.playwright-mcp/gif/` at 1440×900.
2. Caption each frame with a step counter so viewers can follow:
   `magick frame.png -resize 1080x -gravity south -background '#111827'
   -fill white -pointsize 22 -splice 0x44 -annotate +0+10 "3/11 · Stripe
   (dev mock)" cap-frame.png`
3. Assemble: `magick -delay 180 cap-01.png … -delay 250 <frames worth
   reading> -delay 300 cap-last.png -loop 0 -layers optimize
   flow.gif` (delays in 1/100s; hold longer on dense frames;
   `-layers optimize` keeps flat UI GIFs a few hundred KB).
4. Ship it in the same gist as the PNGs — GitHub auto-plays GIFs in PR
   bodies. Embed:
   `![The full flow](…/raw/flow.gif)`

## 3. Host images on a secret gist

The Gist **API is text-only** — `gh gist create/edit` cannot upload PNGs
("binary file not supported"). Binaries go through the gist's **git remote**:

1. New PR: `gh gist create --desc "PR <n> screenshots" README.md` (a stub
   text file), note the gist ID. Existing PR: reuse the ID already in the
   body.
2. Clone it. `gh auth` may be set to ssh, which often fails for gists:
   temporarily `gh config set -h github.com git_protocol https`,
   `gh gist clone <id> <dir>`, then restore
   `gh config set -h github.com git_protocol ssh`.
   The https push then has no credentials of its own, and gists reject the
   ssh remote. Push with the gh token instead:
   `GH_TOKEN=$(gh auth token) git -c credential.helper='!f(){ echo username=<user>; echo password=$GH_TOKEN; };f' push origin HEAD`
3. Copy the PNGs in (same filenames to replace), `git add -A`, commit
   (plain message, no attribution), `git push`.
4. Verify one raw URL serves the new bytes:
   `curl -sI https://gist.githubusercontent.com/<user>/<id>/raw/<file>.png`
   — check 200 + the new content-length.

Embed as
`![alt](https://gist.githubusercontent.com/<user>/<id>/raw/<file>.png)`
— raw URLs **without** a revision hash always serve the latest push.

## 4. Body template

**Short. Reviewers skim.** A body over ~500 words (excluding image URLs)
is too long — cut it before publishing. The rules:

- **One line per bullet.** Fix + why, one clause each. If a bullet needs a
  second sentence, the second sentence is usually the bit to delete.
- **No story paragraph.** Two sentences of intro, maximum: what it is and
  how it works. The GIF already tells the story.
- **Prose that could be bullets, is bullets.** Prose that could be a
  fragment, is a fragment.
- **Group screenshots**, two per caption line, rather than a headed
  paragraph each.
- **Collapse the tail.** Testing and reviewer notes are a few dense lines,
  not a list of lists.
- Keep every *fact* a reviewer needs — the trims are to wording, never to
  content. Known follow-ups and out-of-scope items always stay.

```markdown
## <Feature title> (#<issue, if any>)

![The full flow](gist raw URL of the flow GIF)

<two sentences: what it is, how it works.>

### Included
- one-line bullets: feature + the detail that matters (routes, limits, wording)

### Screenshots
<short caption · second short caption>
![alt](gist raw URL)
![alt](gist raw URL)
(group them in pairs; both sides of a flow where relevant)

### Hardening
- one-line bullets: fix + what it prevents

### Database
- migrations and indexes, or "None." plus why

### Testing
- gates, tests added, what was driven live; link the QA doc. One paragraph.

### Reviewer notes
- out of scope, known follow-ups with doc links. Two or three lines.
```

## 5. Create or update

- New: `gh pr create --title "<conventional title>" --body-file <file>`
  against the default branch.
- Update: `gh pr edit <n> --body-file <file>`. Write the body to a scratch
  file first — never inline a body that size.
- **Verify against the live PR, not the local file**: `gh pr view <n>
  --json body --jq '.body'` and check the new wording is there and the old
  wording is gone. "The edit command exited 0" is not verification.
- If the branch has new commits the user asked to push, push before editing
  so the body matches what reviewers see.
