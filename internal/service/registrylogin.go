package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
)

// The password `stackr registry login` hands to docker: a short-lived token
// that only the registry front accepts (never the CLI's API key), carrying
// what the user may do in the registry at the time it was minted. Docker
// sends it to the front's /token as Basic auth and gets the usual per-repo
// token back.

const (
	registryLoginPrefix = "stackr-rl."
	// RegistryLoginTTL is how long a docker login lasts before the next
	// `stackr registry login`.
	RegistryLoginTTL = 12 * time.Hour
)

type registryLogin struct {
	Sub   string   `json:"sub"`
	Exp   int64    `json:"exp"`
	Admin bool     `json:"admin,omitempty"`
	Orgs  []string `json:"orgs,omitempty"`
	Pull  []string `json:"pull,omitempty"`
}

// RegistryLoginOut is the docker password and when it stops working.
type RegistryLoginOut struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// loginKey is separate from the registry token key, so one kind of token is
// never taken for the other.
func (o *Orchestrator) loginKey() []byte {
	m := hmac.New(sha256.New, []byte(o.cfg.SecretsKey))
	m.Write([]byte("stackr registry login v1"))
	return m.Sum(nil)
}

func (o *Orchestrator) loginSig(payload string) string {
	m := hmac.New(sha256.New, o.loginKey())
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// RegistryLogin mints a docker login password for the caller, scoped to the
// orgs the caller can use in the registry.
func (o *Orchestrator) RegistryLogin(ctx context.Context, by *Principal) (RegistryLoginOut, error) {
	if by == nil {
		return RegistryLoginOut{}, errs.ErrRefused
	}
	f := o.userRegistryFacts(ctx, by)
	exp := time.Now().Add(RegistryLoginTTL)
	b, _ := json.Marshal(registryLogin{
		Sub: f.Subject, Exp: exp.Unix(), Admin: f.Kind == regfront.KindAdmin, Orgs: f.Orgs, Pull: f.PullOrgs,
	})
	p := base64.RawURLEncoding.EncodeToString(b)
	return RegistryLoginOut{Token: registryLoginPrefix + p + "." + o.loginSig(p), ExpiresAt: exp.UTC()}, nil
}

// loginFacts checks a login token and answers the facts it was minted with,
// provided the user is still active.
func (o *Orchestrator) loginFacts(ctx context.Context, tok string) (regfront.Facts, error) {
	p, sig, ok := strings.Cut(strings.TrimPrefix(tok, registryLoginPrefix), ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(o.loginSig(p))) {
		return regfront.Facts{}, errRegistryAuth
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	var c registryLogin
	if err != nil || json.Unmarshal(raw, &c) != nil || time.Now().Unix() >= c.Exp {
		return regfront.Facts{}, errRegistryAuth
	}
	if u, err := o.users.Get(ctx, c.Sub); err != nil || !u.Active {
		return regfront.Facts{}, errRegistryAuth
	}
	f := regfront.Facts{Kind: regfront.KindUser, Subject: c.Sub, Orgs: c.Orgs, PullOrgs: c.Pull}
	if c.Admin {
		f.Kind = regfront.KindAdmin
	}
	return f, nil
}
