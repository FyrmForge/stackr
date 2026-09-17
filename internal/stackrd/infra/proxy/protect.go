package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sync"

	"golang.org/x/crypto/bcrypt"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// basicAuthCost is low on purpose: Traefik checks the hash on every request,
// assets included, and this gates dev sites. htpasswd -B uses the same.
const basicAuthCost = 5

// hashes caches bcrypt output per password so a Resync does not rewrite every
// protected route with a fresh salt, which Traefik would reload each time.
var hashes sync.Map

// basicAuthEntry is the user:hash line for a tile's basicAuth middleware, ""
// when the tile is not protected. The tile's own user and password win over
// the settings cascade (docs/plans/48-install-domains-and-basic-auth.md).
//
// Fails closed: protection on without a user, or with a password reference
// that does not resolve, gets a password nobody knows. The URL is locked,
// never left open. A cascade that cannot be read is an error, not an
// unprotected tile, so the caller can keep the route file it already has.
func (p *Proxy) basicAuthEntry(ctx context.Context, app *repo.Tile) (string, error) {
	on, user, pass := app.BasicAuthUser != "", app.BasicAuthUser, app.BasicAuthPassword
	if !on && p.store != nil {
		r, err := settings.TryTile(ctx, p.store, app)
		if err != nil {
			return "", fmt.Errorf("read protection settings for %s: %w", app.Slug, err)
		}
		on, user, pass = r.Protect, r.ProtectUser, r.ProtectPassword
	}
	if !on {
		return "", nil
	}
	if varref.HasRef(pass) {
		out, err := []string(nil), error(nil)
		if p.store != nil {
			out, err = varref.New(p.store).ExpandStrings(ctx, app.ID, varref.System, []string{pass})
		}
		if p.store == nil || err != nil {
			log.Printf("proxy: %s basic auth password did not resolve, locking it: %v", app.Slug, err)
			pass = ""
		} else {
			pass = out[0]
		}
	}
	if user == "" || pass == "" {
		log.Printf("proxy: %s is protected without a user or password, locking it", app.Slug)
		if user == "" {
			user = "locked"
		}
		return user + ":" + lockHash(), nil
	}
	return user + ":" + hashPassword(pass), nil
}

// lockHash is one bcrypt hash of random bytes nobody has, for the tiles that
// are protected but unusable. Computed once: a fresh hash per render would
// rewrite the route file on every settings save and make Traefik reload it.
var lockHash = sync.OnceValue(func() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	h, err := bcrypt.GenerateFromPassword(b, basicAuthCost)
	if err != nil {
		// 24 bytes is under bcrypt's limit, so this cannot happen; a literal
		// that is not a hash would be compared as plain text by Traefik.
		log.Printf("proxy: lock hash: %v", err)
		return "$2a$05$" + hex.EncodeToString(b)
	}
	return string(h)
})

func hashPassword(pass string) string {
	sum := sha256.Sum256([]byte(pass))
	key := hex.EncodeToString(sum[:])
	if h, ok := hashes.Load(key); ok {
		return h.(string)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pass), basicAuthCost)
	if err != nil {
		// Only fails on a password over 72 bytes, which a ${{ }} reference can
		// expand to. Traefik compares an entry that is not a recognised hash
		// as plain text, so a sentinel like "!" would become the password.
		log.Printf("proxy: basic auth hash, locking the route: %v", err)
		return lockHash()
	}
	hashes.Store(key, string(h))
	return string(h)
}
