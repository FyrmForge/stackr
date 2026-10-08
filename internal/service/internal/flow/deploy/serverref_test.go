package deploy_test

import (
	"io"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
)

// Orgs never read server secrets: a tile that refs server.params fails its
// deploy with a clear error, even when the server value exists, and it is a
// failure, not a park (setting the value would not help).
func TestServerRefRefused(t *testing.T) {
	w := setup(t)
	must(t, w.f.Params.Set(ctx, params.ServerScope, params.Entry{
		Collection: "s3", Name: "secret_key", Kind: params.Secret, Value: "sk",
	}))
	for _, env := range []string{
		`{"KEY":"${{ server.params.s3.secret_key }}"}`,
		`{"KEY":"${{ server.s3.secret_key }}"}`,
	} {
		w.tile.EnvJSON = env
		_, err := w.f.Run(ctx, w.tile, "nginx@sha256:aa", io.Discard, nil)
		if _, parked := errs.IsUnset(err); parked || err == nil {
			t.Fatalf("%s: err = %v, want a refusal", env, err)
		}
		if !strings.Contains(err.Error(), "server") || strings.Contains(err.Error(), "sk") {
			t.Errorf("%s: err = %v", env, err)
		}
		if at(w.fake.Calls(), "Run", "") >= 0 {
			t.Error("a refused tile must not start")
		}
	}
}
