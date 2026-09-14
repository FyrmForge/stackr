package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFuzzyRank(t *testing.T) {
	cases := []struct {
		s, q string
		want int
	}{
		{"site", "site", 0},      // prefix
		{"site-db", "sit", 0},    // prefix
		{"my-site", "site", 1},   // substring elsewhere
		{"site-db", "sdb", 6},    // subsequence: s...db spans four gap chars ("ite-")
		{"site-db", "sitedb", 3}, // subsequence, one gap char ("-")
		{"site", "ste", 3},       // s.te, one gap char
		{"site", "xyz", -1},      // no match
		{"site", "siet", -1},     // out of order
		{"SITE-DB", "sdb", 6},    // case-insensitive
	}
	for _, c := range cases {
		assert.Equal(t, c.want, fuzzyRank(c.s, c.q), "fuzzyRank(%q, %q)", c.s, c.q)
	}
	// ordering property is what the palette relies on: prefix < substring < loose
	assert.False(t, fuzzyRank("site", "sit") >= fuzzyRank("my-site", "sit") || fuzzyRank("my-site", "sit") >= fuzzyRank("s-i-t", "sit"),
		"rank ordering broken: prefix < substring < subsequence expected")
}
