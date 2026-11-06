package store_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A sealed value copied into another row's column does not open there.
func TestSealedValueBoundToRow(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	mustSeedOrg(t, s)
	for _, id := range []string{"c1", "c2"} {
		if err := s.Credentials.Create(ctx, store.Credential{ID: id, OrgID: "o1", Name: id, Password: "pw-" + id, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE credentials SET password = (SELECT password FROM credentials WHERE id = 'c1') WHERE id = 'c2'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Credentials.Get(ctx, "c2"); !errors.Is(err, secrets.ErrDecrypt) {
		t.Errorf("copied value opened: err = %v", err)
	}
	if c, err := s.Credentials.Get(ctx, "c1"); err != nil || c.Password != "pw-c1" {
		t.Errorf("original = %q, %v", c.Password, err)
	}
}

// Sweep moves enc1 values, in columns and settings, to enc2.
func TestSweepMovesLegacyValues(t *testing.T) {
	s := servicetest.Store(t)
	ctx := context.Background()
	mustSeedOrg(t, s)
	if err := s.Credentials.Create(ctx, store.Credential{ID: "c1", OrgID: "o1", Name: "n", Password: "x", CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	kb, _ := hex.DecodeString(servicetest.Key)
	blk, _ := aes.NewCipher(kb)
	gcm, _ := cipher.NewGCM(blk)
	enc1 := func(v string) string {
		n := make([]byte, gcm.NonceSize())
		return "enc1:" + base64.StdEncoding.EncodeToString(gcm.Seal(n, n, []byte(v), nil))
	}
	for _, q := range []struct{ sql, v string }{
		{`UPDATE credentials SET password = ? WHERE id = 'c1'`, "hunter2"},
		{`INSERT INTO settings (key, value) VALUES ('k', ?)`, "setting-v"},
	} {
		if _, err := s.DB().ExecContext(ctx, q.sql, enc1(q.v)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Credentials.Get(ctx, "c1"); err == nil {
		t.Fatal("enc1 opened before the sweep")
	}
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Credentials.Get(ctx, "c1"); err != nil || c.Password != "hunter2" {
		t.Errorf("credential after sweep = %q, %v", c.Password, err)
	}
	if v, err := s.Settings.GetSealed(ctx, "k"); err != nil || v != "setting-v" {
		t.Errorf("setting after sweep = %q, %v", v, err)
	}
	if err := s.Sweep(ctx); err != nil {
		t.Errorf("second sweep: %v", err)
	}
}
