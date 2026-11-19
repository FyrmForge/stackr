package promote

import (
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
)

// A gate login that is a plain variable, or not set, is a plan warning and
// never a blocker; a secret password says nothing.
func TestPlanWarnsOnGate(t *testing.T) {
	w := setup(t)
	sc := params.Scope{Kind: "env", ID: w.dev.ID}
	must(t, w.f.D.Params.Set(ctx, sc, params.Entry{Collection: "gate", Name: "sec", Kind: params.Secret, Value: "s"}))
	for ref, want := range map[string]string{
		"sec":     "",
		"plain":   "gate password is a plain variable",
		"missing": "stays locked until params.gate.missing is set",
	} {
		w.files[ref] = `
version: 1
stack: shop
ladder: [dev, prd]
head: main
params:
  gate:
    dev|prd:
      user: u
      plain: p
      sec: {type: secret}
base:
  tiles:
    api:
      image: nginx:1
      port: 80
      domains:
        - host: api.example.com
          proxy:
            basic_auth:
              user: ${{ params.gate.user }}
              password: ${{ params.gate.` + ref + ` }}
environments:
  dev: {}
  prd: {}
`
		p, err := w.f.Plan(ctx, w.dev.ID, w.release(t, ref).ID, io.Discard)
		must(t, err)
		got := strings.Join(p.Warnings, "|")
		if want == "" && got != "" || !strings.Contains(got, want) || len(p.Blockers) != 0 {
			t.Errorf("%s: warnings %q, blockers %v; want %q", ref, got, p.Blockers, want)
		}
	}
}
