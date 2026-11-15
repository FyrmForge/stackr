package admin

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/FyrmForge/hamr/pkg/respond"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
)

// findCap is how many hits the box lists.
const findCap = 8

// findSections is how many of those slots a matching section is promised.
const findSections = 2

// findMaxQuery is the longest query the box matches, in runes.
const findMaxQuery = 100

// findEntry is one thing the box can find: a catalogue setting (Key set) or
// a section (Key ""). Section is the section's key, SectionLabel its name.
type findEntry struct {
	Key, Label, Help      string
	Section, SectionLabel string
	NoFind                bool // the section's form has no highlight: link without ?find=
}

// GET /-/admin/find?q=: the hits for the Find a setting box, as the
// fragment that fills its results.
func (h *handler) Find(c echo.Context) error {
	q := strings.TrimSpace(c.QueryParam("q"))
	if r := []rune(q); len(r) > findMaxQuery {
		q = string(r[:findMaxQuery])
	}
	var v ui.FindView
	if q != "" {
		es, err := h.findEntries(c)
		if err != nil {
			return middleware.HTTPError(err)
		}
		v = matchFind(q, es)
	}
	return respond.HTML(c, http.StatusOK, ui.FindResults(v))
}

// findEntries is every catalogue knob a section's form shows (knobForms),
// then every section.
func (h *handler) findEntries(c echo.Context) ([]findEntry, error) {
	ss, err := h.orch.ServerSettings(c.Request().Context())
	if err != nil {
		return nil, err
	}
	by := map[string]findEntry{}
	for _, s := range ss {
		by[s.Key] = findEntry{Key: s.Key, Label: s.Label, Help: comp.Sentence(s.Desc)}
	}
	var out []findEntry
	for _, g := range ui.Groups {
		for _, it := range g.Items {
			for _, k := range knobForms[it.Key] {
				if e, ok := by[k]; ok {
					e.Section, e.SectionLabel = it.Key, it.Label
					out = append(out, e)
				}
			}
		}
	}
	for _, k := range ownEditor {
		if e, ok := by[k]; ok {
			e.Section, e.NoFind = ownSection[k], true
			it, _ := ui.Find(e.Section)
			e.SectionLabel = it.Label
			out = append(out, e)
		}
	}
	for _, g := range ui.Groups {
		for _, it := range g.Items {
			out = append(out, findEntry{Label: it.Label, Help: it.Sub, Section: it.Key})
		}
	}
	return out, nil
}

// matchFind is the entries q finds, case-insensitive: label matches, then
// key matches (_ and space alike), then help matches, then sections; stable
// within each, capped at findCap with the rest counted.
func matchFind(q string, es []findEntry) ui.FindView {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return ui.FindView{}
	}
	qk := strings.ReplaceAll(q, "_", " ")
	var tiers [4][]ui.FindHit
	for _, e := range es {
		label, help := strings.ToLower(e.Label), strings.ToLower(e.Help)
		hit := ui.FindHit{Label: marked(e.Label, q), Help: marked(e.Help, q), Section: e.SectionLabel, Href: ui.Href(e.Section)}
		tier := -1
		switch {
		case e.Key == "":
			// a section matches on its name or its line
			if strings.Contains(label, q) || strings.Contains(help, q) {
				tier = 3
			}
		case strings.Contains(label, q):
			tier = 0
		case strings.Contains(strings.ReplaceAll(strings.ToLower(e.Key), "_", " "), qk):
			tier = 1
		case strings.Contains(help, q):
			tier = 2
		}
		if tier < 0 {
			continue
		}
		if e.Key != "" && !e.NoFind {
			hit.Href += "?find=" + url.QueryEscape(e.Key)
		}
		tiers[tier] = append(tiers[tier], hit)
	}
	var settings []ui.FindHit
	for _, t := range tiers[:3] {
		settings = append(settings, t...)
	}
	secs := tiers[3]
	// up to findSections slots are held for sections, so a matching section
	// is never pushed out by settings
	room := findCap - min(len(secs), findSections)
	if len(settings) < room {
		room = len(settings)
	}
	hits := append(settings[:room:room], secs[:min(len(secs), findCap-room)]...)
	return ui.FindView{Total: len(settings) + len(secs), Hits: hits, Asked: true}
}

// marked is s in pieces, the lower-case q's matches flagged. A string whose
// lower case changes length is left unmarked.
func marked(s, q string) []ui.FindPart {
	low := strings.ToLower(s)
	if len(low) != len(s) {
		return []ui.FindPart{{Text: s}}
	}
	var out []ui.FindPart
	for {
		i := strings.Index(low, q)
		if i < 0 {
			break
		}
		if i > 0 {
			out = append(out, ui.FindPart{Text: s[:i]})
		}
		out = append(out, ui.FindPart{Text: s[i : i+len(q)], Hit: true})
		s, low = s[i+len(q):], low[i+len(q):]
	}
	if s != "" {
		out = append(out, ui.FindPart{Text: s})
	}
	return out
}
