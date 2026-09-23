package testdb

import (
	"context"
	"database/sql"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Links satisfies config/sharelink.Links by writing the store directly.
//
// The real implementation is service.RevokeService, which owns the
// secret_links table on both sides now. config/sharelink is a package
// service/ is built on, so its in-package tests cannot name it.
//
// Same caveat as the other doubles here: the methods it stands in for carry
// no rule today. The mint-time scope grant that D-8 is really about is not
// one of them yet; when it lands, these tests should fail rather than keep
// passing against a double that skips it.
type Links struct{ Store repo.Store }

func (l Links) Mint(ctx context.Context, link *repo.SecretLink) error {
	return l.Store.CreateSecretLink(ctx, link)
}

func (l Links) ByHash(ctx context.Context, tokenHash string) (*repo.SecretLink, error) {
	return l.Store.GetSecretLinkByHash(ctx, tokenHash)
}

func (l Links) Claim(ctx context.Context, id, state string) (bool, error) {
	return l.Store.ClaimSecretLink(ctx, id, state)
}

func (l Links) BurnDrop(ctx context.Context, id string, vars []repo.Variable) (bool, error) {
	return l.Store.BurnDropLink(ctx, id, vars)
}

func (l Links) Touch(ctx context.Context, id string, attempts int, openedAt sql.NullTime) error {
	return l.Store.TouchSecretLink(ctx, id, attempts, openedAt)
}
