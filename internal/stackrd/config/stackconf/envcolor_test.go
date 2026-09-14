package stackconf

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const colored = `
version: 1
stack: demo
base:
  tiles:
    web:
      image: web:1
      env: {PORT: "8080"}
environments:
  dev:
    color: teal
  production:
    color: "#112233"
    tiles:
      web:
        image: web:2
        env: {PORT: "80"}
`

func TestEnvColorAndDeclaredKeys(t *testing.T) {
	r, err := Load([]byte(colored), nil)
	require.NoError(t, err)
	assert.Equal(t, "teal", r.Envs["dev"].Color)
	assert.Equal(t, "#112233", r.Envs["production"].Color)
	assert.Equal(t, map[string]map[string]bool{"web": {"image": true, "env.PORT": true}}, r.Envs["production"].Declared,
		"what the env overlay sets, flattened, so the compare panel can mark it intended")
	assert.Nil(t, r.Envs["dev"].Declared)

	_, err = Load([]byte(colored+"  staging:\n    color: red\n"), nil)
	require.Error(t, err, "red is neither a palette name nor a hex")

	// a colour change alone is a plan row, so it lands on apply
	p := Diff(r, State{Envs: map[string]EnvState{
		"dev":        {Tiles: map[string]TileState{}, Color: "teal"},
		"production": {Tiles: map[string]TileState{}, Color: ""},
	}}, DiffOpts{})
	var colorRows []Change
	for _, ch := range p.Changes {
		if ch.Kind == "update-env" {
			colorRows = append(colorRows, ch)
		}
	}
	require.Len(t, colorRows, 1)
	assert.Equal(t, Change{Kind: "update-env", Env: "production", Field: "color", Old: "default", New: "#112233"}, colorRows[0])
}
