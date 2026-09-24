# config/secrets

- **Source**: `config/secrets/secrets.go`, `config/secrets/secrets_test.go`
- **Commit**: c2423f0
- **Taken**: AES-GCM encryption at rest — key loading, `Encrypt`, `Decrypt`, the `enc:` storage encoding, the wrong-key failure paths, the round-trip test.
- **Cut**: `Generate` (charset policy for `default: generated`), `RandomHex`, `Key()` (master-key hex export), `TestRandomHex`.
- **Cuts belong to**: `Generate` → `leaf/params` (generated secret values, REWRITE.md "As today, no decision needed"). `RandomHex` → nowhere; it is `hex.EncodeToString` over `crypto/rand`, one line each caller re-derives, no row need carry it. `Key()` → probably dead: REWRITE.md:545 makes archives `age`-encrypted, so nothing needs the master key in hex. Check that before rebuilding it; if something does, backup owns it.

Target package: `service/internal/secrets`.

## Kept code

```go
// Package secrets encrypts sensitive values at rest with AES-GCM. The master
// key lives in <dataDir>/keys/master.key (0600), created on first boot;
// STACKR_MASTER_KEY (64 hex chars) overrides it so the key can live apart
// from the database.
package secrets // imports omitted: all stdlib, regenerate from the code below

const prefix = "enc:"

var gcm cipher.AEAD

// Load initializes the cipher from STACKR_MASTER_KEY or the key file at path,
// generating the file if absent.
func Load(path string) error {
	var key []byte
	if h := os.Getenv("STACKR_MASTER_KEY"); h != "" {
		k, err := hex.DecodeString(h)
		if err != nil || len(k) != 32 {
			return fmt.Errorf("STACKR_MASTER_KEY must be 64 hex chars")
		}
		key = k
	} else {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			k, derr := hex.DecodeString(strings.TrimSpace(string(b)))
			if derr != nil || len(k) != 32 {
				return fmt.Errorf("invalid master key file %s", path)
			}
			key = k
		case os.IsNotExist(err):
			key = make([]byte, 32)
			if _, rerr := rand.Read(key); rerr != nil {
				return rerr
			}
			if merr := os.MkdirAll(filepath.Dir(path), 0o700); merr != nil {
				return merr
			}
			if werr := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); werr != nil {
				return werr
			}
		default:
			return err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err = cipher.NewGCM(block)
	if err != nil {
		return err
	}
	return nil
	// extract: dropped `master = key` and `func Key() string` — they existed only
	// so a panel backup could write the master hex back out verbatim, and
	// age-encrypted archives likely make that dead. See the header.
}

// Encrypt returns "enc:<base64(nonce||ciphertext)>". Empty and already
// encrypted input pass through unchanged.
func Encrypt(s string) string {
	if s == "" || strings.HasPrefix(s, prefix) || gcm == nil {
		return s // see Notes: this is the fail-open, fix it in the rewrite
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	ct := gcm.Seal(nonce, nonce, []byte(s), nil)
	return prefix + base64.StdEncoding.EncodeToString(ct)
}

// Decrypt reverses Encrypt. Values without the prefix (legacy plaintext) pass
// through unchanged.
func Decrypt(s string) (string, error) {
	if !strings.HasPrefix(s, prefix) {
		return s, nil // see Notes: no legacy plaintext exists, make this an error
	}
	if gcm == nil {
		return "", fmt.Errorf("secrets: cipher not initialized")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("secrets: ciphertext too short")
	}
	pt, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// extract: dropped `RandomHex(n int) string` (hex.EncodeToString over
// crypto/rand, panicking on failure), belongs nowhere — a one-liner its callers
// (db/registry passwords, webhook and invite tokens) re-derive.
// extract: dropped `Generate(length int, numbers, symbols bool) string` and its
// shell/URL/YAML-safe charset, belongs in leaf/params — the charset is the
// domain rule behind `default: generated`, not an encryption concern.
```

```go
func TestRoundTrip(t *testing.T) {
	require.NoError(t, Load(filepath.Join(t.TempDir(), "master.key")))
	enc := Encrypt("KEY=value\nMULTI=\"a\nb\"")
	require.True(t, strings.HasPrefix(enc, "enc:"), "not encrypted: %q", enc)
	require.Equal(t, enc, Encrypt(enc), "double encryption")
	dec, err := Decrypt(enc)
	require.NoError(t, err)
	require.Equal(t, "KEY=value\nMULTI=\"a\nb\"", dec)
	require.Equal(t, "", Encrypt(""), "empty should pass through")
	// extract: dropped the plaintext-passthrough assertion, see Notes.
}

// Not in the source; the row asks for it. Write it against whatever holds the
// cipher after the globals go away.
func TestWrongKey(t *testing.T) {
	require.NoError(t, Load(filepath.Join(t.TempDir(), "a.key")))
	enc := Encrypt("hunter2")
	t.Setenv("STACKR_MASTER_KEY", strings.Repeat("ab", 32))
	require.NoError(t, Load(filepath.Join(t.TempDir(), "b.key")))
	_, err := Decrypt(enc)
	require.Error(t, err, "a wrong key must fail, not return garbage")
}

// extract: dropped TestRandomHex, follows RandomHex to its callers.
```

## Notes for the builder

**Key source.** `STACKR_MASTER_KEY` (64 hex chars, 32 bytes) wins; otherwise the
hex in `<dataDir>/keys/master.key`, mode 0600, dir 0700. No KDF — raw random
bytes, not a passphrase, so no salt and no work factor to tune.

Third path, a decision and not a detail: on a **missing** key file `Load` mints a
fresh key and writes it. Right on first boot, wrong on an unmounted volume — it
succeeds, and every stored secret is undecryptable against the new key with no
error anywhere. Pick explicit init (`stackr init` writes it, `Load` only reads)
or keep auto-create knowingly.

**Encrypt fails open — the real defect here.** With `gcm == nil` (`Load` never
ran, or its error was swallowed) `Encrypt` returns the secret *unencrypted and
unprefixed*, it is stored as plaintext, and `Decrypt` sees no prefix and returns
it with a `nil` error. Nothing complains anywhere in the chain. `Decrypt` errors
on `gcm == nil`, `Encrypt` does not: that asymmetry is the bug, and the cause is
the signature — `Encrypt(string) string` has nowhere to put an error, because the
nonce path panics instead. Give `Encrypt` an error return, or drop the package
globals so `Load` hands back a value carrying the cipher and "encrypt before
load" becomes unsayable. The globals are the mechanism, not a style question.

**How a wrong key fails**, all four paths:
- wrong 32-byte key → `gcm.Open` fails the auth tag → error, never partial plaintext;
- bad `STACKR_MASTER_KEY` or a corrupt key file → `Load` errors, no cipher is ever built;
- truncated value → explicit `secrets: ciphertext too short` before `Open`;
- non-base64 body → the decoder's error.

**Storage encoding.** `enc:` + `base64.StdEncoding(nonce || ciphertext)`, nonce
12 bytes from `gcm.NonceSize()`, tag inside the ciphertext. The prefix is *not* a
version — there is no version field at all, so a key rotation or an algorithm
change cannot tell old bytes from new. Compatibility does not matter (no installs
exist), so spend the character now: write `enc1:` and refuse anything else. Free
for the same reason: `Decrypt`'s unprefixed passthrough exists for legacy
plaintext rows that do not exist — make a missing prefix an error, else a
corrupted prefix silently returns ciphertext-ish bytes as the secret.

Size: source 195 lines, extract 181 lines
