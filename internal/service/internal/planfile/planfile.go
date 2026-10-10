// Package planfile is what the org file and the server file share: the
// plan's shape (Change, Plan), the approve contract (impact lines, removal
// ticks), the strict YAML decode and the params grammar. A sibling of slug:
// flows and the service import it, it imports no leaf and no flow.
// flow/promote's stack file uses the decode, Entry and CheckParams.
package planfile

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
)

// Change is one line of a plan. Kind is the flow's own vocabulary; Tile
// names the thing the line is about (a stack, a host, a dest).
//
// Impact is the human line for a risky change ("panel moves to X; point DNS
// first"); a plan with any Impact is risky and its approve needs a confirm.
// Optional marks a removal row: the file no longer names something live. It
// applies only when the approver ticks it, by Key, a stable "<kind>:<key>"
// (route:<host>, dest:<name>, domain:<host>, share:<slug>,
// connector-share:<conn>/<org>, org-binding:<slug>).
type Change struct {
	Kind     string `json:"kind"`
	Tile     string `json:"tile,omitempty"`
	Field    string `json:"field,omitempty"`
	Old      string `json:"old,omitempty"`
	New      string `json:"new,omitempty"`
	Note     string `json:"note,omitempty"`
	Impact   string `json:"impact,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	Key      string `json:"key,omitempty"`
}

// Line is the change as one job-log line: "update api port: 80 -> 8080 (note)".
// An empty old, new or field leaves no gap: "create c -> image", "volume data".
func (c Change) Line() string {
	s := c.Kind
	if c.Tile != "" {
		s += " " + c.Tile
	}
	if c.Field != "" {
		s += " " + c.Field
		if c.Old != "" {
			s += ":"
		}
	}
	switch {
	case c.Old != "" && c.New != "", (c.Old != "" || c.New != "") && (c.Tile != "" || c.Field != ""):
		s += " " + strings.TrimSpace(c.Old+" -> "+c.New)
	case c.Old != "" || c.New != "":
		s += " " + c.Old + c.New
	}
	if c.Note != "" {
		s += " (" + c.Note + ")"
	}
	return s
}

// Plan is what applying a file would do. Blockers refuse the apply; Notes
// do not. A flow embeds it and adds its Summary kinds.
type Plan struct {
	Changes  []Change `json:"changes"`
	Blockers []string `json:"blockers,omitempty"`
	Notes    []string `json:"notes,omitempty"`
}

func (p Plan) Blocked() bool { return len(p.Blockers) > 0 }

// Block adds a blocker.
func (p *Plan) Block(format string, a ...any) {
	p.Blockers = append(p.Blockers, fmt.Sprintf(format, a...))
}

// Add appends a change.
func (p *Plan) Add(c Change) { p.Changes = append(p.Changes, c) }

// Risky says a change carries an impact line.
func (p Plan) Risky() bool {
	return slices.ContainsFunc(p.Changes, func(c Change) bool { return c.Impact != "" })
}

// Impacts are the impact lines, in plan order.
func (p Plan) Impacts() []string {
	var out []string
	for _, c := range p.Changes {
		if c.Impact != "" {
			out = append(out, c.Impact)
		}
	}
	return out
}

// Removals are the removal rows, in plan order.
func (p Plan) Removals() []Change {
	var out []Change
	for _, c := range p.Changes {
		if c.Optional {
			out = append(out, c)
		}
	}
	return out
}

// AutoOK says the plan may apply by itself: no blocker, no impact line, no
// removal row. An empty plan is.
func (p Plan) AutoOK() bool {
	return !p.Blocked() && !p.Risky() && len(p.Removals()) == 0
}

// TickedRemovals are the removal rows keys tick, in plan order. A key the
// plan no longer holds (the re-diff dropped it) is not there.
func (p Plan) TickedRemovals(keys []string) []Change {
	var out []Change
	for _, c := range p.Removals() {
		if slices.Contains(keys, c.Key) {
			out = append(out, c)
		}
	}
	return out
}

// OnlyTicked is the plan an apply walks: every plain change, and of the
// removal rows only the ticked ones. Unticked removals never apply, so an
// apply that forgets to filter cannot delete.
func (p Plan) OnlyTicked(keys []string) Plan {
	out := p
	out.Changes = nil
	for _, c := range p.Changes {
		if !c.Optional || slices.Contains(keys, c.Key) {
			out.Changes = append(out.Changes, c)
		}
	}
	return out
}

// Approvable is the approve contract, one rule for the org plan and the
// server plan: a risky plan needs confirm, and every ticked key must be a
// removal row of this plan. The service calls it; no surface decides alone.
func (p Plan) Approvable(ticked []string, confirm bool) error {
	removals := map[string]bool{}
	for _, c := range p.Removals() {
		removals[c.Key] = true
	}
	for _, k := range ticked {
		if !removals[k] {
			return errs.Invalidf("ticked", "%q is not a removal in this plan.", k)
		}
	}
	if p.Risky() && !confirm {
		return errs.Conflictf("This plan has impact lines; confirm to approve.")
	}
	return nil
}

// Summary is "2 to add, 1 to change, 2 removals to review, needs confirm, 1
// blocker". isAdd says which kinds add; removal rows count only as removals.
func (p Plan) Summary(isAdd func(kind string) bool) string {
	var add, change, remove int
	for _, c := range p.Changes {
		switch {
		case c.Optional:
			remove++
		case isAdd(c.Kind):
			add++
		default:
			change++
		}
	}
	var parts []string
	if add > 0 {
		parts = append(parts, fmt.Sprintf("%d to add", add))
	}
	if change > 0 {
		parts = append(parts, fmt.Sprintf("%d to change", change))
	}
	switch remove {
	case 0:
	case 1:
		parts = append(parts, "1 removal to review")
	default:
		parts = append(parts, fmt.Sprintf("%d removals to review", remove))
	}
	if len(parts) == 0 {
		parts = append(parts, "no changes")
	}
	if p.Risky() {
		parts = append(parts, "needs confirm")
	}
	switch n := len(p.Blockers); {
	case n == 1:
		parts = append(parts, "1 blocker")
	case n > 1:
		parts = append(parts, fmt.Sprintf("%d blockers", n))
	}
	return strings.Join(parts, ", ")
}

// Param is one params: declaration. A secret is name and type only: the
// file is in git.
type Param struct {
	Type  string  `yaml:"type"` // param | secret
	Value *string `yaml:"value,omitempty"`
	// Generate is a secret's length: stackr makes the value once, the first
	// time the env has none, and never changes it.
	Generate int `yaml:"generate,omitempty"`
}

// IsRef reports whether s is exactly one ${{ ... }} reference and nothing
// else; a mixed value like "${{ a.b }}x" carries a literal and is not.
func IsRef(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "${{") && strings.HasSuffix(s, "}}") && strings.Count(s, "${{") == 1 && strings.Count(s, "}}") == 1
}

// CheckWide is the one-value-per-name params: grammar (the server file only;
// org files use CheckParams). generate: is acted on only by the stack file
// (allowGenerate).
func CheckWide(ps map[string]map[string]Param, allowGenerate bool) error {
	for c, entries := range ps {
		if !slug.ValidName(c) {
			return fmt.Errorf("params: collection %q is lower-case letters, digits and _", c)
		}
		for n, p := range entries {
			switch {
			case !slug.ValidName(n):
				return fmt.Errorf("params: %s.%s: a name is lower-case letters, digits and _", c, n)
			case p.Generate != 0 && !allowGenerate:
				return fmt.Errorf("params: %s.%s: generate is only read in a stack file", c, n)
			case p.Generate != 0 && p.Type != "secret":
				return fmt.Errorf("params: %s.%s: generate is only for type: secret", c, n)
			case p.Generate != 0 && p.Value != nil:
				return fmt.Errorf("params: %s.%s: generate and value are exclusive", c, n)
			case p.Generate != 0 && (p.Generate < 16 || p.Generate > 128):
				return fmt.Errorf("params: %s.%s: generate is a length from 16 to 128", c, n)
			case p.Type == "secret" && p.Value != nil:
				return fmt.Errorf("params: %s.%s is a secret; its value never goes in the file", c, n)
			case p.Type != "secret" && p.Type != "param":
				return fmt.Errorf("params: %s.%s: type must be param or secret", c, n)
			}
		}
	}
	return nil
}

var unknownField = regexp.MustCompile(`^(?:line \d+: )?field (\S+) not found in type \S+$`)

// StrictYAML decodes refusing unknown keys, top level included. removed
// tells an old file what replaced a key (nil = no hints).
func StrictYAML(data []byte, out any, removed map[string]string) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && err != io.EOF {
		return humanYAML(err, removed)
	}
	return nil
}

// humanYAML rewrites yaml.v3's unknown-key errors: they name a Go type and
// count lines in a re-marshalled fragment, which means nothing to the
// person editing the file.
func humanYAML(err error, removed map[string]string) error {
	te, ok := err.(*yaml.TypeError)
	if !ok {
		return err
	}
	msgs := make([]string, 0, len(te.Errors))
	for _, e := range te.Errors {
		m := unknownField.FindStringSubmatch(e)
		if m == nil {
			msgs = append(msgs, e)
			continue
		}
		s := "unknown key " + m[1]
		if hint := removed[m[1]]; hint != "" {
			s += " (no longer supported: " + hint + ")"
		}
		msgs = append(msgs, s)
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}

// KeepSecretPair settles protect_user + protect_password of a file's defaults
// rung against live. Exports leave a literal password out, so a file that
// omits the pair means "untouched": both are taken from live. An explicit
// empty pair means "clear": both become nil, the form a cleared rung has.
func KeepSecretPair(user, pass **string, liveUser, livePass *string) {
	switch {
	case *user == nil && *pass == nil:
		*user, *pass = liveUser, livePass
	case *user != nil && *pass != nil && **user == "" && **pass == "":
		*user, *pass = nil, nil
	}
}
