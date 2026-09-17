package project

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// A declared secret the last settled plan found unset is a not-set row until
// someone sets it at the stack or the org. Org-scope inputs belong to the org
// panel, and a stack with no plan has nothing declared.
func TestUnsetSecrets(t *testing.T) {
	cp := &repo.ConfigPlan{Plan: `{"changes":[],"inputs":[
		{"scope":"stack","name":"SMTP_PASSWORD","secret":true,"blocked":["prod/web"]},
		{"scope":"stack","name":"STRIPE_KEY","secret":true},
		{"scope":"stack","name":"SENTRY_DSN","secret":true},
		{"scope":"org","name":"ORG_TOKEN","secret":true}]}`}

	got := unsetSecrets(cp,
		[]repo.Variable{{Name: "STRIPE_KEY"}},
		[]repo.Variable{{Name: "SENTRY_DSN"}},
		nil)
	require.Len(t, got, 1)
	require.Equal(t, "SMTP_PASSWORD", got[0].Name)
	require.Equal(t, []string{"prod/web"}, got[0].Blocked)

	require.Nil(t, unsetSecrets(nil, nil, nil, nil))
}

// A stack-declared secret may be valued per environment: varref resolves the
// consumer's env row before the stack one, so a name set in every environment
// is set, and the page must stop calling it "not set".
func TestUnsetSecretsPerEnv(t *testing.T) {
	cp := &repo.ConfigPlan{Plan: `{"inputs":[
		{"scope":"stack","name":"API_KEY","secret":true},
		{"scope":"stack","name":"DB_PASS","secret":true}]}`}
	names := func(byEnv map[string][]repo.Variable, stackVars, orgVars []repo.Variable) []string {
		var out []string
		for _, in := range unsetSecrets(cp, stackVars, orgVars, byEnv) {
			out = append(out, in.Name)
		}
		return out
	}
	v := func(name string) []repo.Variable { return []repo.Variable{{Name: name, Secret: true}} }

	require.Equal(t, []string{"API_KEY", "DB_PASS"}, names(nil, nil, nil))

	both := map[string][]repo.Variable{"staging": v("API_KEY"), "prod": v("API_KEY")}
	require.Equal(t, []string{"DB_PASS"}, names(both, nil, nil))

	// Missing from one environment is still a deploy that will not resolve.
	one := map[string][]repo.Variable{"staging": v("API_KEY"), "prod": nil}
	require.Equal(t, []string{"API_KEY", "DB_PASS"}, names(one, nil, nil))

	// A stack or org row covers every environment at once.
	require.Equal(t, []string{"DB_PASS"}, names(one, v("API_KEY"), nil))
	require.Equal(t, []string{"DB_PASS"}, names(one, nil, v("API_KEY")))
}
