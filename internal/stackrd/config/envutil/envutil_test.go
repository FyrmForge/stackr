package envutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	got := Parse("A=1\n\n# c\nB=\"line1\nline2\"\nC='x'\nD=\"unterminated\nrest")
	want := []Var{
		{"A", "1"},
		{"B", "line1\nline2"},
		{"C", "x"},
		{"D", "unterminated\nrest"},
	}
	require.Equal(t, want, got)
}

func TestInject(t *testing.T) {
	// append when absent
	env, used := Inject("FOO=bar", "DATABASE_URL", "u1")
	assert.Equal(t, "FOO=bar\nDATABASE_URL=u1", env, "append")
	assert.Equal(t, "DATABASE_URL", used, "append")
	// idempotent when already set to the same value
	env, used = Inject("DATABASE_URL=u1", "DATABASE_URL", "u1")
	assert.Equal(t, "DATABASE_URL=u1", env, "idempotent")
	assert.Equal(t, "DATABASE_URL", used, "idempotent")
	// don't clobber a different existing value, suffix instead
	env, used = Inject("DATABASE_URL=old", "DATABASE_URL", "new")
	assert.Equal(t, "DATABASE_URL_2", used, "collision")
	assert.Equal(t, "DATABASE_URL=old\nDATABASE_URL_2=new", env, "collision")
	// second collision escalates to _3
	_, used = Inject("DATABASE_URL=a\nDATABASE_URL_2=b", "DATABASE_URL", "c")
	assert.Equal(t, "DATABASE_URL_3", used, "escalate")
}

func TestLines(t *testing.T) {
	got := Lines("A=1\nB=\"a\nb\"")
	want := []string{"A=1", "B=a\nb"}
	require.Equal(t, want, got)
}
