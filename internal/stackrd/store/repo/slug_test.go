package repo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"My Shop":        "my-shop",
		"  API Server! ": "api-server",
		"main_postgres":  "main-postgres",
		"Ütf8 Ñame":      "tf8-ame",
		"a--b":           "a-b",
	}
	for in, want := range cases {
		assert.Equal(t, want, Slugify(in), "Slugify(%q)", in)
	}
}
