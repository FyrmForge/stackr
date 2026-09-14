package stackconf

import (
	"fmt"
	"sort"
	"strings"
)

// ParseDep decodes one depends_on line: "slug" or "slug:condition".
// Bare slug means started.
func ParseDep(line string) (slug, cond string, err error) {
	slug, cond = line, "started"
	if i := strings.IndexByte(line, ':'); i >= 0 {
		slug, cond = line[:i], line[i+1:]
	}
	if slug == "" {
		return "", "", fmt.Errorf("depends_on %q: empty tile slug", line)
	}
	switch cond {
	case "started", "healthy", "completed":
		return slug, cond, nil
	}
	return "", "", fmt.Errorf("depends_on %q: condition must be started, healthy or completed", line)
}

// validateDeps checks one environment's startup-order graph: every target
// exists in the env, conditions fit the target's kind, no self-deps, no
// cycles. Pure, runs at parse for the file path and inside ApplyResolved for
// staged edits, which bypass resolve().
func validateDeps(envName string, tiles map[string]TileConf) error {
	for name, tc := range tiles {
		for _, line := range tc.DependsOn {
			slug, cond, err := ParseDep(line)
			if err != nil {
				return fmt.Errorf("env %s tile %s: %w", envName, name, err)
			}
			if slug == name {
				return fmt.Errorf("env %s tile %s: depends on itself", envName, name)
			}
			dep, ok := tiles[slug]
			if !ok {
				return fmt.Errorf("env %s tile %s: depends_on %q: no such tile in this environment", envName, name, slug)
			}
			if cond == "completed" {
				if dep.Type != "cron" && dep.Type != "function" {
					return fmt.Errorf("env %s tile %s: depends_on %s:completed; completed only applies to cron and function tiles", envName, name, slug)
				}
				// A function that never runs on deploy can't complete during an
				// apply, that dependency would only ever time out.
				if dep.Type == "function" && !dep.RunOnDeploy {
					return fmt.Errorf("env %s tile %s: depends_on %s:completed needs run_on_deploy: true on %s", envName, name, slug, slug)
				}
			}
		}
	}
	// Cycle check: DFS with colors over the declared edges.
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var visit func(n string) error
	visit = func(n string) error {
		color[n] = grey
		stack = append(stack, n)
		for _, line := range tiles[n].DependsOn {
			slug, _, _ := ParseDep(line)
			if _, ok := tiles[slug]; !ok {
				continue
			}
			switch color[slug] {
			case grey:
				return fmt.Errorf("env %s: depends_on cycle through %s", envName, strings.Join(append(stack, slug), " → "))
			case white:
				if err := visit(slug); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return nil
	}
	names := make([]string, 0, len(tiles))
	for n := range tiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if color[n] == white {
			if err := visit(n); err != nil {
				return err
			}
		}
	}
	return nil
}

// topoDeps reorders one applyOrder pass so a tile follows its depends_on
// targets. Kahn with an alphabetical frontier keeps the walk reproducible;
// edges to slugs outside the set (unchanged, already existing tiles) are
// ignored; a cycle (validated upstream, but staged patches can race) falls
// back to the incoming order rather than dropping tiles.
func topoDeps(slugs []string, re ResolvedEnv) []string {
	in := map[string]bool{}
	for _, s := range slugs {
		in[s] = true
	}
	indeg := map[string]int{}
	dependents := map[string][]string{} // dep -> tiles waiting on it
	for _, s := range slugs {
		indeg[s] = 0
	}
	for _, s := range slugs {
		for _, line := range re.Tiles[s].DependsOn {
			dep, _, err := ParseDep(line)
			if err != nil || !in[dep] || dep == s {
				continue
			}
			indeg[s]++
			dependents[dep] = append(dependents[dep], s)
		}
	}
	frontier := make([]string, 0, len(slugs))
	for _, s := range slugs {
		if indeg[s] == 0 {
			frontier = append(frontier, s)
		}
	}
	sort.Strings(frontier)
	out := make([]string, 0, len(slugs))
	for len(frontier) > 0 {
		s := frontier[0]
		frontier = frontier[1:]
		out = append(out, s)
		next := dependents[s]
		sort.Strings(next)
		for _, d := range next {
			indeg[d]--
			if indeg[d] == 0 {
				frontier = append(frontier, d)
				sort.Strings(frontier)
			}
		}
	}
	if len(out) != len(slugs) {
		return slugs // cycle, keep incoming order
	}
	return out
}
