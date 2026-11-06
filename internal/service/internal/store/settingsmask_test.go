package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

func TestMaskAndKeepSecret(t *testing.T) {
	stored := `{"protect":true,"protect_user":"u","protect_password":"hunter2"}`
	masked := store.MaskSettings(stored)
	if strings.Contains(masked, "hunter2") || !strings.Contains(masked, store.SecretMask) || !strings.Contains(masked, `"protect_user":"u"`) {
		t.Fatalf("masked = %s", masked)
	}
	// Written back as read: the stored password stays. A new one replaces it.
	if got := store.KeepSecret(stored, masked); !strings.Contains(got, "hunter2") {
		t.Errorf("round trip lost the password: %s", got)
	}
	if got := store.KeepSecret(stored, `{"protect_password":"new"}`); !strings.Contains(got, `"new"`) {
		t.Errorf("a new password was not taken: %s", got)
	}
	for _, blob := range []string{`{}`, `{"protect_password":""}`, `not json`} {
		if store.MaskSettings(blob) != blob {
			t.Errorf("MaskSettings(%q) changed a blob with nothing to hide", blob)
		}
	}
}

// An org's settings blob is sealed at rest and read back whole; a plaintext
// row from before reads as it is.
func TestOrgSettingsSealedAtRest(t *testing.T) {
	st := servicetest.Store(t)
	ctx := context.Background()
	o := store.Org{ID: "o1", Name: "o", Slug: "o", EnvColors: "{}", Settings: `{"protect_password":"hunter2"}`, CreatedAt: time.Now()}
	if err := st.Orgs.Create(ctx, o); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.DB().QueryRowContext(ctx, `SELECT settings FROM orgs WHERE id = 'o1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "hunter2") || !strings.HasPrefix(raw, "enc2:") {
		t.Fatalf("stored = %q", raw)
	}
	got, err := st.Orgs.Get(ctx, "o1")
	if err != nil || got.Settings != o.Settings {
		t.Fatalf("read back = %q, %v", got.Settings, err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE orgs SET settings = '{"protect":true}' WHERE id = 'o1'`); err != nil {
		t.Fatal(err)
	}
	if got, err := st.Orgs.Get(ctx, "o1"); err != nil || got.Settings != `{"protect":true}` {
		t.Fatalf("plaintext row = %q, %v", got.Settings, err)
	}
}
