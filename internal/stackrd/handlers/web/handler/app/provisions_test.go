package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEnvVarName(t *testing.T) {
	cases := map[string]string{
		"DATABASE_URL": "DATABASE_URL",
		"db url":       "DBURL",
		"pg-url":       "PGURL",
		"9lives":       "LIVES", // leading digit dropped
		"  db  ":       "DB",
		"!!!":          "",
	}
	for in, want := range cases {
		assert.Equal(t, want, envVarName(in), "envVarName(%q)", in)
	}
}

func TestInjectEnvVar(t *testing.T) {
	// fresh key appends, name unchanged
	env, used := injectEnvVar("FOO=bar", "DATABASE_URL", "u1")
	assert.Equal(t, "FOO=bar\nDATABASE_URL=u1", env, "append")
	assert.Equal(t, "DATABASE_URL", used, "append")
	// same key + same value is a no-op (idempotent re-provision)
	env, used = injectEnvVar("DATABASE_URL=u1", "DATABASE_URL", "u1")
	assert.Equal(t, "DATABASE_URL=u1", env, "idempotent")
	assert.Equal(t, "DATABASE_URL", used, "idempotent")
	// collision: key holds a DIFFERENT value → must NOT clobber, suffix instead
	env, used = injectEnvVar("DATABASE_URL=old", "DATABASE_URL", "new")
	assert.Equal(t, "DATABASE_URL_2", used, "collision")
	assert.Equal(t, "DATABASE_URL=old\nDATABASE_URL_2=new", env, "collision")
	// second collision escalates to _3
	_, used = injectEnvVar("DATABASE_URL=a\nDATABASE_URL_2=b", "DATABASE_URL", "c")
	assert.Equal(t, "DATABASE_URL_3", used, "collision escalate")
}
