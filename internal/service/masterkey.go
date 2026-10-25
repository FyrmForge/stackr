package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// masterGenFile holds the master key generation and when it began, two lines.
// It is information for the panel and the restore warning, never a key.
const masterGenFile = "keys/master.gen"

// LoadMasterKey is the key the process runs on: the env key when set, else
// keys/master.key. A keys/master.key.next left by a rotation is finished here,
// before anything serves, by the canary setting (sealed with the master key,
// moved in the same transaction as every other sealed value):
//
//	opens with the current key: the transaction never committed, so run it now
//	opens with the next key:    it committed and the rename did not, so rename
//
// A failed transaction rolls back whole, the next key is dropped and the
// current key stays.
func LoadMasterKey(ctx context.Context, dataDir, dbPath, envKey string) (string, error) {
	if envKey != "" {
		return envKey, nil
	}
	curPath := filepath.Join(dataDir, installspec.KeyFile)
	nextPath := filepath.Join(dataDir, installspec.KeyNextFile)
	b, _ := os.ReadFile(curPath)
	cur := strings.TrimSpace(string(b))
	nb, err := os.ReadFile(nextPath)
	if errors.Is(err, os.ErrNotExist) {
		return cur, nil
	}
	if err != nil {
		return "", err
	}
	next := strings.TrimSpace(string(nb))
	drop := func(why string, err error) (string, error) {
		slog.Warn("master key rotation dropped; the current key stays", "why", why, "error", err)
		return cur, removeIfThere(nextPath)
	}
	boxCur, err := secrets.New(cur)
	if err != nil {
		return "", err
	}
	boxNext, err := secrets.New(next)
	if err != nil || next == cur {
		return drop("the next key is not a new key", err)
	}
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "stackr.db")
	}
	db, err := openDB(ctx, dbPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	stCur, stNext := store.New(db, boxCur), store.New(db, boxNext)

	if _, err := stCur.Settings.GetSealed(ctx, store.CanaryKey); err != nil {
		// Not the current key's canary: committed under the next key, or a
		// mismatch neither key explains.
		if v, nerr := stNext.Settings.GetSealed(ctx, store.CanaryKey); nerr != nil || v != store.CanaryValue {
			return "", errors.New("neither keys/master.key nor keys/master.key.next opens this database")
		}
	} else {
		if err := ensureCanary(ctx, stCur); err != nil {
			return "", err
		}
		if err := stCur.Reseal(ctx, boxCur, boxNext); err != nil {
			return drop("re-sealing failed, nothing changed", err)
		}
	}
	gen, _ := MasterGen(dataDir)
	if err := os.Rename(nextPath, curPath); err != nil {
		return "", err
	}
	// ponytail: a crash between the rename and this write leaves the
	// generation one low; it is shown, never relied on.
	if err := installspec.WriteKeyFile(filepath.Join(dataDir, masterGenFile),
		[]byte(strconv.Itoa(gen+1)+"\n"+time.Now().UTC().Format(time.RFC3339)+"\n")); err != nil {
		slog.Warn("master key generation not recorded", "error", err)
	}
	return next, nil
}

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// MasterGen is the master key's generation (1 until the first rotation) and
// when it began (zero before then).
func MasterGen(dataDir string) (int, time.Time) {
	b, err := os.ReadFile(filepath.Join(dataDir, masterGenFile))
	if err != nil {
		return 1, time.Time{}
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 1, time.Time{}
	}
	n, err := strconv.Atoi(f[0])
	if err != nil || n < 1 {
		return 1, time.Time{}
	}
	at, _ := time.Parse(time.RFC3339, f[1])
	return n, at
}

// ensureCanary writes the canary on a database that has none, and fails when
// the one it has does not open with the loaded key: a wrong key says so at
// boot, not on the first sealed read.
func ensureCanary(ctx context.Context, st *store.Store) error {
	v, err := st.Settings.GetSealed(ctx, store.CanaryKey)
	if err != nil {
		return fmt.Errorf("the master key does not open this database: %w", err)
	}
	if v != "" {
		return nil
	}
	return st.Settings.SetSealed(ctx, store.CanaryKey, store.CanaryValue)
}

// MasterKeyStatus is what Server settings, Keys shows about the master key.
type MasterKeyStatus struct {
	Gen     int
	Since   time.Time // zero = never rotated
	FromEnv bool      // set by STACKR_MASTER_KEY: cannot be rotated here
	Pending bool      // a rotation is written and waits for the restart
}

func (o *Orchestrator) MasterKeyStatus() MasterKeyStatus {
	gen, since := MasterGen(o.cfg.DataDir)
	_, err := os.Stat(filepath.Join(o.cfg.DataDir, installspec.KeyNextFile))
	return MasterKeyStatus{Gen: gen, Since: since, FromEnv: o.cfg.KeyFromEnv, Pending: err == nil}
}

// RotateMasterKey writes a new key next to the current one and restarts the
// panel behind the admin's password. The next boot re-seals every secret
// (LoadMasterKey) before serving.
func (o *Orchestrator) RotateMasterKey(ctx context.Context, userID, password string) error {
	if err := o.users.VerifyPassword(ctx, userID, password); err != nil {
		return err
	}
	if o.cfg.KeyFromEnv {
		return errs.Refusedf("the key is set by STACKR_MASTER_KEY; change it where it is set")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	if err := installspec.WriteKeyFile(filepath.Join(o.cfg.DataDir, installspec.KeyNextFile), []byte(hex.EncodeToString(raw)+"\n")); err != nil {
		return err
	}
	if o.cfg.Restart != nil {
		// After the answer is out.
		time.AfterFunc(2*time.Second, o.cfg.Restart)
	}
	return nil
}
