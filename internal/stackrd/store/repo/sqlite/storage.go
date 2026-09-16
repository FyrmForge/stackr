package sqlite

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Storage rows carry SMB credentials, password is encrypted at rest, same
// pattern as provisions.db_password. (Docker's volume metadata still holds a
// second plaintext copy once a volume is created; §2.7 accepts that.)

const storageCols = `id, server_id, org_id, name, slug, backend, address, export, username, password, opts, status, status_msg, created_at`

// storageSelect reads the two owner columns as "" rather than NULL: exactly
// one is set, and the model carries plain strings.
const storageSelect = `SELECT id, COALESCE(server_id, '') AS server_id, COALESCE(org_id, '') AS org_id, name, slug, backend,
	address, export, username, password, opts, status, status_msg, created_at FROM storage`

func (s *Store) CreateStorage(ctx context.Context, st *repo.Storage) error {
	enc := *st
	enc.Password = secrets.Encrypt(st.Password)
	if enc.ServerID == "" && enc.OrgID == "" {
		enc.ServerID = "local"
	}
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO storage (`+storageCols+`) VALUES
		 (:id, NULLIF(:server_id, ''), NULLIF(:org_id, ''), :name, :slug, :backend, :address, :export, :username, :password, :opts, :status, :status_msg, :created_at)`, &enc)
	return err
}

func decryptStorage(st *repo.Storage, err error) (*repo.Storage, error) {
	if err != nil || st == nil {
		return st, err
	}
	st.Password, err = secrets.Decrypt(st.Password)
	return st, err
}

func (s *Store) GetStorage(ctx context.Context, id string) (*repo.Storage, error) {
	return decryptStorage(get[repo.Storage](ctx, s, storageSelect+` WHERE id = ?`, id))
}

// GetStorageBySlug finds a server pool or share. Org shares are not found
// here: their slugs are only unique inside one org (GetOrgStorageBySlug).
func (s *Store) GetStorageBySlug(ctx context.Context, slug string) (*repo.Storage, error) {
	return decryptStorage(get[repo.Storage](ctx, s, storageSelect+` WHERE slug = ? AND server_id IS NOT NULL`, slug))
}

func (s *Store) GetOrgStorageBySlug(ctx context.Context, orgID, slug string) (*repo.Storage, error) {
	return decryptStorage(get[repo.Storage](ctx, s, storageSelect+` WHERE org_id = ? AND slug = ?`, orgID, slug))
}

func (s *Store) ListStorage(ctx context.Context) ([]repo.Storage, error) {
	sts, err := list[repo.Storage](ctx, s, storageSelect+` ORDER BY name`)
	if err != nil {
		return sts, err
	}
	for i := range sts {
		if sts[i].Password, err = secrets.Decrypt(sts[i].Password); err != nil {
			return sts, err
		}
	}
	return sts, nil
}

func (s *Store) UpdateStorage(ctx context.Context, st *repo.Storage) error {
	enc := *st
	enc.Password = secrets.Encrypt(st.Password)
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE storage SET name = :name, backend = :backend, address = :address, export = :export,
		 username = :username, password = :password, opts = :opts, status = :status, status_msg = :status_msg
		 WHERE id = :id`, &enc)
	return err
}

func (s *Store) DeleteStorage(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM storage WHERE id = ?`, id)
	return err
}

func (s *Store) CreateStoragePath(ctx context.Context, p *repo.StoragePath) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO storage_paths (id, storage_id, name, subpath, forced_ro, created_at)
		 VALUES (:id, :storage_id, :name, :subpath, :forced_ro, :created_at)`, p)
	return err
}

func (s *Store) GetStoragePath(ctx context.Context, id string) (*repo.StoragePath, error) {
	return get[repo.StoragePath](ctx, s, `SELECT * FROM storage_paths WHERE id = ?`, id)
}

func (s *Store) ListStoragePaths(ctx context.Context, storageID string) ([]repo.StoragePath, error) {
	return list[repo.StoragePath](ctx, s, `SELECT * FROM storage_paths WHERE storage_id = ? ORDER BY name`, storageID)
}

func (s *Store) DeleteStoragePath(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM storage_paths WHERE id = ?`, id)
	return err
}
