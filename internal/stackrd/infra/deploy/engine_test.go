package deploy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/stream"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/agent"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func TestSplitLines(t *testing.T) {
	got := splitLines("FOO=bar\n\n# comment\n  BAZ=qux  \n")
	want := []string{"FOO=bar", "BAZ=qux"}
	require.Equal(t, want, got)
}

func TestSplitCommand(t *testing.T) {
	got, err := splitCommand(`-config.file=/etc/loki/config.yml`)
	require.NoError(t, err)
	require.Equal(t, []string{"-config.file=/etc/loki/config.yml"}, got)

	got, err = splitCommand(`--a "one two" 'three "four"' esc\ aped`)
	require.NoError(t, err)
	require.Equal(t, []string{"--a", "one two", `three "four"`, "esc aped"}, got)

	got, err = splitCommand("")
	require.NoError(t, err)
	require.Empty(t, got)

	_, err = splitCommand(`"unterminated`)
	require.Error(t, err, "unterminated quote must fail, not silently truncate")
}

func TestParseKV(t *testing.T) {
	got := parseKV("A=1\nB=x=y\nnovalue\n")
	require.Equal(t, "1", got["A"], "got %v", got)
	require.Equal(t, "x=y", got["B"], "got %v", got)
	_, ok := got["novalue"]
	require.False(t, ok, "keyless line should be dropped: %v", got)
}

// NewEngine took a *cluster.Cluster and dropped it on the floor: the field was
// added, the parameter was added, and the struct literal in between was not,
// so e.clus was nil in every running panel. Nothing caught it. The compiler is
// happy with an unused parameter, no unit test builds a real engine, and the
// first call through it is a deploy's image push, which nil-panicked inside
// the work queue's recover and left the deployment saying "running" for ever.
func TestNewEngineKeepsWhatItIsGiven(t *testing.T) {
	store := testdb.New(t)
	clus := cluster.New(nil, agent.New(nil, "", t.TempDir()), store)
	rows := storeRows{s: store}
	e := NewEngine(store, nil, clus, stream.NewHub(), t.TempDir(), nil, rows)

	require.NotNil(t, e.clus, "the cluster is every docker call a deploy makes")
	assert.Same(t, clus, e.clus)
	assert.Same(t, store, e.store)
	assert.Equal(t, rows, e.rows, "the row owners a deploy writes an environment's overlay through")
}

// The build context and dockerfile come from a tile's settings and the clone
// lives one level below every tile's deploy keys and rendered files. A
// context of "../../keys" with a one-line COPY Dockerfile would ship them all
// in a pullable image (docs/plans/37-builds.md).
func TestBuildPathsCannotLeaveTheRepo(t *testing.T) {
	repo := "/data/repos/tile"
	for _, p := range []string{"../../keys", "..", "sub/../../other", "../tile2"} {
		_, err := underRepo(repo, p)
		assert.Error(t, err, p)
	}
	for _, p := range []string{".", "", "app", "..hidden", "/abs/is/rooted/in/repo", "a/../b"} {
		_, err := underRepo(repo, p)
		assert.NoError(t, err, p)
	}
}
