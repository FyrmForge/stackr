package operated

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
)

// PlanHashIn is everything a recipe plan's hash covers (R21, V5): a plan is
// stale the moment any of it moves. Apply recomputes the hash from live state
// and the stored plan's inputs and refuses a mismatch.
type PlanHashIn struct {
	// Names are the requested recipe names; Recipes the resolved recipes
	// (the requires closure) with their full definitions, so an edit of a
	// recipe or of one checksum makes the plan stale.
	Names   []string
	Recipes []Recipe
	// Pins are the resolved package=version pins.
	Pins map[string]string
	// Base is the official image the build sits on; Arch the target CPU type
	// and NodeID the node the build and the instance run on.
	Base, Arch, NodeID string
	// ImageRef, Preload, Extensions and ExtBuild are the instance's current
	// values (ExtBuild is the raw column).
	ImageRef            string
	Preload, Extensions []string
	ExtBuild            string
}

// planHashDoc is the canonical document that gets hashed.
type planHashDoc struct {
	Names      []string          `json:"names"`
	Recipes    []Recipe          `json:"recipes"`
	Pins       map[string]string `json:"pins"`
	Base       string            `json:"base"`
	Arch       string            `json:"arch"`
	NodeID     string            `json:"node"`
	ImageRef   string            `json:"image"`
	Preload    []string          `json:"preload"`
	Extensions []string          `json:"extensions"`
	ExtBuild   any               `json:"ext_build"`
}

// canonRecipe orders every field that is a set (map keys are sorted by the
// JSON encoder): two recipes that differ only in list order hash the same.
// Preload keeps its order, it is the library load order.
func canonRecipe(r Recipe) Recipe {
	r.Majors = sorted(r.Majors)
	r.Packages = sorted(r.Packages)
	r.Extensions = sorted(r.Extensions)
	r.Requires = slices.Clone(r.Requires) // order: it sets the derived preload order
	r.Preload = slices.Clone(r.Preload)
	r.SHA256 = maps.Clone(r.SHA256)
	return r
}

func sorted[S ~[]E, E interface{ ~int | ~string }](s S) S {
	s = slices.Clone(s)
	slices.Sort(s)
	return slices.Compact(s)
}

// PlanHash is the plan's hash: SHA-256 over the canonical JSON of its inputs,
// hex. The same inputs always give the same hash, whatever the order of the
// recipes, names, extensions or map entries.
func PlanHash(in PlanHashIn) string {
	rs := make([]Recipe, 0, len(in.Recipes))
	for _, r := range in.Recipes {
		rs = append(rs, canonRecipe(r))
	}
	slices.SortFunc(rs, func(a, b Recipe) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	doc := planHashDoc{
		Names:      sorted(in.Names),
		Recipes:    rs,
		Pins:       in.Pins,
		Base:       in.Base,
		Arch:       in.Arch,
		NodeID:     in.NodeID,
		ImageRef:   in.ImageRef,
		Preload:    slices.Clone(in.Preload),
		Extensions: sorted(in.Extensions),
	}
	// The ext_build column is hashed as its parsed record, so a column
	// rewritten with the same content does not make a plan stale; text that
	// does not parse is hashed as it is.
	if b, err := ParseExtBuild(in.ExtBuild); err == nil {
		doc.ExtBuild = b
	} else {
		doc.ExtBuild = in.ExtBuild
	}
	// nil and empty lists and maps must hash alike.
	doc.Names = nonNil(doc.Names)
	doc.Preload = nonNil(doc.Preload)
	doc.Extensions = nonNil(doc.Extensions)
	if doc.Pins == nil {
		doc.Pins = map[string]string{}
	}
	out, err := json.Marshal(doc)
	if err != nil { // cannot happen: strings, lists and maps of strings
		panic("operated: plan hash: " + err.Error())
	}
	sum := sha256.Sum256(append([]byte("pgextplan/v1\n"), out...))
	return hex.EncodeToString(sum[:])
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
