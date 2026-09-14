package stackconf

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yaml "go.yaml.in/yaml/v3"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

// files: accepts the decided map form and the line form; both normalize to
// canonical sorted lines so rows and diffs agree.
func TestFileListYAMLForms(t *testing.T) {
	var m struct {
		Files FileList `yaml:"files"`
	}
	require.NoError(t, yaml.Unmarshal([]byte("files:\n  config/loki.yml: /etc/loki/config.yml\n  conf.yml: /app/conf.yml:template\n"), &m))
	assert.Equal(t, FileList{"conf.yml:/app/conf.yml:template", "config/loki.yml:/etc/loki/config.yml"}, m.Files)

	var l struct {
		Files FileList `yaml:"files"`
	}
	require.NoError(t, yaml.Unmarshal([]byte("files:\n  - config/loki.yml:/etc/loki/config.yml\n"), &l))
	assert.Equal(t, FileList{"config/loki.yml:/etc/loki/config.yml"}, l.Files)

	var bad struct {
		Files FileList `yaml:"files"`
	}
	require.Error(t, yaml.Unmarshal([]byte("files: nope\n"), &bad))
}

func TestParseFileMount(t *testing.T) {
	rp, cp, tmpl, err := runtime.ParseFileMount("config/loki.yml:/etc/loki/config.yml")
	require.NoError(t, err)
	assert.True(t, rp == "config/loki.yml" && cp == "/etc/loki/config.yml" && !tmpl)

	rp, cp, tmpl, err = runtime.ParseFileMount("conf.yml:/app/conf.yml:template")
	require.NoError(t, err)
	assert.True(t, rp == "conf.yml" && cp == "/app/conf.yml" && tmpl)

	for _, bad := range []string{"../../etc/passwd:/x", "/abs:/x", "a", "a:relative", ":/x"} {
		_, _, _, err := runtime.ParseFileMount(bad)
		assert.Error(t, err, bad)
	}
}
