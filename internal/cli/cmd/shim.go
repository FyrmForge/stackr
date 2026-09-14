package cmd

import "strings"

// tileActions are the canonical tile sub-verbs. Anything else in the slot
// after `tile` is read as a tile ref for the legacy id-first form.
var tileActions = map[string]bool{
	"create": true, "attach": true, "provisions": true, "domain": true,
	"volume": true, "rm": true, "set": true, "run": true, "ls": true,
}

// rootBoolFlags are flags that stand alone in raw argv, the shim needs to
// know which tokens are values when hunting for the first two positionals.
// Only flags that can plausibly precede the tile verb matter here.
var rootBoolFlags = map[string]bool{
	"--json": true, "--yes": true, "-y": true, "--help": true, "-h": true,
	"--no-https": true, "--cron": true, "--public": true, "--secret": true,
	"--force": true, "--follow": true, "--disabled": true,
	"--detailed-exitcode": true, "--include-env-on-default": true,
	"--with-key": true,
}

// RewriteArgs is the pre-cobra compatibility shim. It translates:
//   - `tile <ref> <action> …` → `tile <action> <ref> …` (legacy id-first form)
//   - `-with-key` → `--with-key` (single-dash long the old parser accepted;
//     pflag would read it as bundled shorthands)
//
// It runs on raw argv (without argv[0]) before cobra parses anything.
func RewriteArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if a == "-with-key" {
			out[i] = "--with-key"
		}
		if a == "--" {
			break
		}
	}
	if len(out) == 0 || (out[0] != "tile" && out[0] != "apps") {
		return out
	}
	// Find the first three positional tokens after `tile`, skipping flags and
	// their values. Stop at `--`.
	idx := []int{}
	for i := 1; i < len(out) && len(idx) < 3; i++ {
		a := out[i]
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") {
			if !rootBoolFlags[a] && !strings.Contains(a, "=") {
				i++ // this flag consumes the next token
			}
			continue
		}
		idx = append(idx, i)
	}
	if len(idx) < 2 {
		return out
	}
	first, second := out[idx[0]], out[idx[1]]
	if tileActions[first] || !tileActions[second] {
		return out
	}
	if (second == "domain" || second == "volume") && len(idx) == 3 {
		// `tile <ref> domain add …` → `tile domain add <ref> …`: the ref
		// belongs after the sub-verb, not between the two.
		out[idx[0]], out[idx[1]], out[idx[2]] = second, out[idx[2]], first
		return out
	}
	out[idx[0]], out[idx[1]] = second, first
	return out
}
