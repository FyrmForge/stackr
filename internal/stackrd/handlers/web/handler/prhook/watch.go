package prhook

import (
	"regexp"
	"strings"
)

// watchMatch reports whether a push's changed files should deploy a tile.
// watch is one regex per line; a "!" prefix makes it an ignore rule. A file
// counts when it matches no ignore rule and (there are no positive rules, or
// it matches one). Empty watch config or an unknown change set (empty list,
// force pushes) always deploys. Invalid regex lines are skipped.
func watchMatch(watch string, changed []string) bool {
	var pos, neg []*regexp.Regexp
	for _, line := range strings.Split(watch, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ignore := strings.HasPrefix(line, "!")
		re, err := regexp.Compile(strings.TrimPrefix(line, "!"))
		if err != nil {
			continue
		}
		if ignore {
			neg = append(neg, re)
		} else {
			pos = append(pos, re)
		}
	}
	if len(pos) == 0 && len(neg) == 0 {
		return true
	}
	if len(changed) == 0 {
		return true // payload carried no file list, assume anything changed
	}
file:
	for _, f := range changed {
		for _, re := range neg {
			if re.MatchString(f) {
				continue file
			}
		}
		if len(pos) == 0 {
			return true
		}
		for _, re := range pos {
			if re.MatchString(f) {
				return true
			}
		}
	}
	return false
}
