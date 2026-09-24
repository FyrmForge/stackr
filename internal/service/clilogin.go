package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// CLICodeTTL is how long a CLI login code waits for its exchange.
const CLICodeTTL = 2 * time.Minute

type cliCode struct {
	userID, orgID, name string
	exp                 time.Time
}

// cliCodes are the pending CLI logins.
// ponytail: in memory, one process; a restart drops pending logins and the
// CLI asks again. A table if stackrd ever runs twice.
type cliCodes struct {
	mu sync.Mutex
	m  map[string]cliCode
}

// CLICode is the browser half of CLI login: the signed-in user approves,
// and gets a one-time code for the CLI to exchange. The code, not the key,
// passes through the browser.
func (o *Orchestrator) CLICode(ctx context.Context, userID, orgID, name string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := hex.EncodeToString(b)
	c := &o.cli
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.m == nil {
		c.m = map[string]cliCode{}
	}
	for k, v := range c.m {
		if now.After(v.exp) {
			delete(c.m, k)
		}
	}
	c.m[code] = cliCode{
		userID: userID,
		orgID:  orgID,
		name:   name,
		exp:    now.Add(CLICodeTTL),
	}
	return code, nil
}

// ExchangeCLICode is the CLI half: a code works once, before it expires,
// and becomes a key bound to the code's org with the user's live role
// (MintKey, B36).
func (o *Orchestrator) ExchangeCLICode(ctx context.Context, code string) (string, APIKey, error) {
	c := &o.cli
	c.mu.Lock()
	v, ok := c.m[code]
	delete(c.m, code)
	c.mu.Unlock()
	if !ok || time.Now().After(v.exp) {
		return "", APIKey{}, errs.Invalidf("code", "login code is unknown, used or expired")
	}
	return o.MintKey(ctx, v.userID, v.orgID, v.name)
}
