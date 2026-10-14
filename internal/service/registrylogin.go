package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/regfront"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The password `stackr registry login` hands to docker: a short-lived token
// that only the registry front accepts (never the CLI's API key), carrying
// only who minted it (user and key id): the rights are read from live rows on
// every /token, so a demotion or a revoked key is felt at once. Docker
// sends it to the front's /token as Basic auth and gets the usual per-repo
// token back.

const (
	registryLoginPrefix = "stackr-rl."
	// RegistryLoginTTL is how long a docker login lasts before the next
	// `stackr registry login`.
	RegistryLoginTTL = 12 * time.Hour
)

type registryLogin struct {
	Sub string `json:"sub"`
	Key string `json:"key,omitempty"` // the API key it was minted from, "" for a session
	Exp int64  `json:"exp"`
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

// RegistryLogin mints a docker login password for the caller.
func (o *Orchestrator) RegistryLogin(ctx context.Context, by *Principal) (RegistryLoginOut, error) {
	if by == nil {
		return RegistryLoginOut{}, errs.ErrRefused
	}
	exp := time.Now().Add(RegistryLoginTTL)
	b, _ := json.Marshal(registryLogin{Sub: by.User.ID, Key: by.KeyID, Exp: exp.Unix()})
	p := base64.RawURLEncoding.EncodeToString(b)
	return RegistryLoginOut{Token: registryLoginPrefix + p + "." + o.loginSig(p), ExpiresAt: exp.UTC()}, nil
}

// loginFacts checks a login token and answers the facts of its subject now:
// the user must still be active and a key it came from must still exist.
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
	u, err := o.users.Get(ctx, c.Sub)
	if err != nil || !u.Active {
		return regfront.Facts{}, errRegistryAuth
	}
	var k *store.APIKey
	if c.Key != "" {
		key, err := o.store.APIKeys.Get(ctx, c.Key)
		if err != nil || key.UserID != u.ID {
			return regfront.Facts{}, errRegistryAuth
		}
		k = &key
	}
	pr, err := o.principal(ctx, u, k)
	if err != nil {
		return regfront.Facts{}, errRegistryAuth
	}
	if k != nil {
		pr.KeyID = k.ID
	}
	if authz.Self(pr.Access) != nil {
		return regfront.Facts{}, errRegistryAuth
	}
	return o.userRegistryFacts(ctx, pr), nil
}
