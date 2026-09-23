package sharelink

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func mintDrop(t *testing.T, s *sqlite.Store, pass string, ttl time.Duration) (*repo.SecretLink, string) {
	t.Helper()
	fields, err := EncodeFields([]repo.SecretLinkField{
		{Name: "STRIPE_KEY", Secret: true},
		{Name: "STRIPE_ACCOUNT", Secret: false},
	})
	require.NoError(t, err)
	l := &repo.SecretLink{
		Kind: repo.LinkDrop, OwnerKind: repo.OwnerStack, OwnerID: "stack1",
		Fields: fields, ExpiresAt: time.Now().Add(ttl), CreatedBy: "user1",
	}
	token, err := Mint(context.Background(), s, testdb.Links{Store: s}, l, pass)
	require.NoError(t, err)
	return l, token
}

func TestDropSubmitWritesVarsAndBurns(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	testdb.SeedStack(t, s, false)
	_, token := mintDrop(t, s, "", time.Hour)

	l, err := Open(ctx, testdb.Links{Store: s}, token)
	require.NoError(t, err)
	vals := map[string]string{"STRIPE_KEY": "sk_live_x", "STRIPE_ACCOUNT": "acct_1"}
	require.NoError(t, Submit(ctx, s, testdb.Links{Store: s}, l, vals))

	vars, err := s.ListVariables(ctx, repo.OwnerStack, "stack1")
	require.NoError(t, err)
	require.Len(t, vars, 2)
	byName := map[string]repo.Variable{}
	for _, v := range vars {
		byName[v.Name] = v
	}
	require.Equal(t, "sk_live_x", byName["STRIPE_KEY"].Value)
	require.True(t, byName["STRIPE_KEY"].Secret)
	require.False(t, byName["STRIPE_ACCOUNT"].Secret)

	// Burned: the token no longer opens, and a replayed submit writes nothing.
	_, err = Open(ctx, testdb.Links{Store: s}, token)
	require.ErrorIs(t, err, ErrDead)
	require.ErrorIs(t, Submit(ctx, s, testdb.Links{Store: s}, l, map[string]string{
		"STRIPE_KEY": "sk_live_evil", "STRIPE_ACCOUNT": "acct_evil"}), ErrDead)
	vars, err = s.ListVariables(ctx, repo.OwnerStack, "stack1")
	require.NoError(t, err)
	require.Len(t, vars, 2)
	require.Equal(t, "sk_live_x", vars[1].Value) // STRIPE_KEY, ordered by name
}

func TestDropPartialSubmitDoesNotBurn(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	testdb.SeedStack(t, s, false)
	_, token := mintDrop(t, s, "", time.Hour)

	l, err := Open(ctx, testdb.Links{Store: s}, token)
	require.NoError(t, err)
	require.Error(t, Submit(ctx, s, testdb.Links{Store: s}, l, map[string]string{"STRIPE_KEY": "sk_live_x"}))

	// Still usable, a client who missed a field can come back and finish.
	l, err = Open(ctx, testdb.Links{Store: s}, token)
	require.NoError(t, err)
	require.NoError(t, Submit(ctx, s, testdb.Links{Store: s}, l, map[string]string{
		"STRIPE_KEY": "sk_live_x", "STRIPE_ACCOUNT": "acct_1"}))
}

func TestExpiredLinkIsDeadWhileStateStillOpen(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	testdb.SeedStack(t, s, false)
	l, token := mintDrop(t, s, "", -time.Minute)

	require.Equal(t, repo.LinkOpen, l.State) // no sweep has run
	_, err := Open(ctx, testdb.Links{Store: s}, token)
	require.ErrorIs(t, err, ErrDead)
}

func TestPassphraseLocksAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	testdb.SeedStack(t, s, false)
	_, token := mintDrop(t, s, "hunter2", time.Hour)

	for i := 1; i < repo.MaxLinkAttempts; i++ {
		l, err := Open(ctx, testdb.Links{Store: s}, token)
		require.NoError(t, err)
		require.ErrorIs(t, Unlock(ctx, testdb.Links{Store: s}, l, "nope"), ErrPass)
	}
	l, err := Open(ctx, testdb.Links{Store: s}, token)
	require.NoError(t, err)
	require.ErrorIs(t, Unlock(ctx, testdb.Links{Store: s}, l, "nope"), ErrDead)

	// Locked stays locked, even for someone who now knows the passphrase.
	_, err = Open(ctx, testdb.Links{Store: s}, token)
	require.ErrorIs(t, err, ErrDead)
}

func TestPassphraseAcceptedAndNotCountedAgainstAttempts(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	testdb.SeedStack(t, s, false)
	_, token := mintDrop(t, s, "hunter2", time.Hour)

	l, err := Open(ctx, testdb.Links{Store: s}, token)
	require.NoError(t, err)
	require.ErrorIs(t, Unlock(ctx, testdb.Links{Store: s}, l, "nope"), ErrPass)
	l, err = Open(ctx, testdb.Links{Store: s}, token)
	require.NoError(t, err)
	require.NoError(t, Unlock(ctx, testdb.Links{Store: s}, l, "hunter2"))
}

func mintShare(t *testing.T, s *sqlite.Store, window int) (*repo.SecretLink, string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	require.NoError(t, s.UpsertVariable(ctx, &repo.Variable{
		OwnerKind: repo.OwnerStack, OwnerID: "stack1", Name: "DB_URL",
		Value: "postgres://x", Secret: true, CreatedAt: now, UpdatedAt: now}))
	fields, err := EncodeFields([]repo.SecretLinkField{{Name: "DB_URL", Secret: true}})
	require.NoError(t, err)
	l := &repo.SecretLink{
		Kind: repo.LinkShare, OwnerKind: repo.OwnerStack, OwnerID: "stack1",
		Fields: fields, WindowMinutes: window,
		ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "user1",
	}
	token, err := Mint(ctx, s, testdb.Links{Store: s}, l, "")
	require.NoError(t, err)
	return l, token
}

func TestShareRevealsLiveValueAndBurns(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	testdb.SeedStack(t, s, false)
	_, token := mintShare(t, s, 0)

	l, err := Open(ctx, testdb.Links{Store: s}, token)
	require.NoError(t, err)
	vars, err := Reveal(ctx, s, testdb.Links{Store: s}, l)
	require.NoError(t, err)
	require.Len(t, vars, 1)
	require.Equal(t, "postgres://x", vars[0].Value)

	_, err = Open(ctx, testdb.Links{Store: s}, token)
	require.ErrorIs(t, err, ErrDead)
}

func TestShareWindowStaysOpenThenDies(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	testdb.SeedStack(t, s, false)
	_, token := mintShare(t, s, 10)

	l, err := Open(ctx, testdb.Links{Store: s}, token)
	require.NoError(t, err)
	_, err = Reveal(ctx, s, testdb.Links{Store: s}, l)
	require.NoError(t, err)

	// Second read inside the window still works, that is what the grace is for.
	l, err = Open(ctx, testdb.Links{Store: s}, token)
	require.NoError(t, err)
	_, err = Reveal(ctx, s, testdb.Links{Store: s}, l)
	require.NoError(t, err)

	// Once the window has closed the link is dead without any sweep.
	l.OpenedAt.Time = time.Now().Add(-11 * time.Minute)
	require.NoError(t, s.TouchSecretLink(ctx, l.ID, l.Attempts, l.OpenedAt))
	_, err = Open(ctx, testdb.Links{Store: s}, token)
	require.ErrorIs(t, err, ErrDead)
}

func TestRevokeKillsLink(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	testdb.SeedStack(t, s, false)
	l, token := mintDrop(t, s, "", time.Hour)

	require.NoError(t, Revoke(ctx, testdb.Links{Store: s}, l.ID))
	_, err := Open(ctx, testdb.Links{Store: s}, token)
	require.ErrorIs(t, err, ErrDead)
}

func TestUnknownTokenIsDead(t *testing.T) {
	s := testdb.New(t)
	_, err := Open(context.Background(), testdb.Links{Store: s}, "deadbeef")
	require.ErrorIs(t, err, ErrDead)
}
