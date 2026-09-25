package githubapp

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestSignJWT(t *testing.T) {
	key, pemKey := testKey(t)
	jwt, err := signJWT("42", pemKey)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("parts: %d", len(parts))
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, h[:], sig); err != nil {
		t.Fatalf("verify: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss      string
		Iat, Exp int64
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Iss != "42" || claims.Exp-claims.Iat != 600 {
		t.Fatalf("claims: %+v %v", claims, err)
	}
	if _, err := signJWT("42", "not a key"); err == nil {
		t.Fatal("bad key accepted")
	}
}

func TestToken(t *testing.T) {
	_, pemKey := testKey(t)
	var mints int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			http.Error(w, "no jwt", http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /app/installations":
			_, _ = w.Write([]byte(`[{"id":7}]`))
		case "POST /app/installations/7/access_tokens":
			mints++
			exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			_, _ = w.Write([]byte(`{"token":"ghs_x","expires_at":"` + exp + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New("https://stackr.example.com")
	c.apiURL = srv.URL
	app := App{ID: 1, Slug: "s", PEM: pemKey}
	for range 2 {
		if tok, err := c.Token(context.Background(), "conn1", app); err != nil || tok != "ghs_x" {
			t.Fatalf("token: %q %v", tok, err)
		}
	}
	if mints != 1 {
		t.Fatalf("minted %d times, want 1 (cached)", mints)
	}
}

func TestNotInstalledAndRepos(t *testing.T) {
	_, pemKey := testKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /app/installations":
			_, _ = w.Write([]byte(`[]`))
		case "GET /installation/repositories":
			if r.Header.Get("Authorization") != "Bearer ghs_x" {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"total_count":1,"repositories":[{"full_name":"acme/api","private":true,"default_branch":"main"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New("https://stackr.example.com")
	c.apiURL = srv.URL
	if _, err := c.Token(context.Background(), "conn1", App{ID: 1, Slug: "s", PEM: pemKey}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("no installations = %v, want ErrNotInstalled", err)
	}
	rs, err := c.Repos(context.Background(), "ghs_x")
	if err != nil || len(rs) != 1 || rs[0].FullName != "acme/api" || !rs[0].Private {
		t.Fatalf("repos = %+v %v", rs, err)
	}
}

func TestManifestAndConvert(t *testing.T) {
	c := New("https://stackr.example.com/")
	action, m, err := c.Manifest("abcdef12", "acme", "abcdef12.nonce")
	if err != nil || action != "https://github.com/organizations/acme/settings/apps/new?state=abcdef12.nonce" {
		t.Fatalf("action: %q %v", action, err)
	}
	var got struct {
		Name string
		Hook struct{ URL string } `json:"hook_attributes"`
		Evts []string             `json:"default_events"`
	}
	if err := json.Unmarshal([]byte(m), &got); err != nil || got.Name != "stackr-stackr.example.com-abcd" ||
		got.Hook.URL != "https://stackr.example.com/hooks/connectors/abcdef12" || len(got.Evts) != 2 {
		t.Fatalf("manifest: %+v %v", got, err)
	}
	if _, _, err := c.Manifest("ab", "", "x"); err == nil {
		t.Fatal("short connector id accepted")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app-manifests/code1/conversions" || r.Header.Get("Authorization") != "" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":5,"slug":"s","pem":"p","webhook_secret":"w"}`))
	}))
	defer srv.Close()
	c.apiURL = srv.URL
	if app, err := c.ConvertManifest(context.Background(), "code1"); err != nil || app.ID != 5 ||
		app.WebhookSecret != "w" {
		t.Fatalf("convert: %+v %v", app, err)
	}
	if _, err := c.ConvertManifest(context.Background(), "used"); err == nil {
		t.Fatal("non-201 accepted")
	}
}

func TestCloneAuth(t *testing.T) {
	env := CloneAuth("https://github.com/acme/shop", "tok")
	if len(env) != 3 || !strings.HasSuffix(env[2], base64.StdEncoding.EncodeToString([]byte("x-access-token:tok"))) {
		t.Fatalf("env: %v", env)
	}
	if CloneAuth("https://gitlab.com/acme/shop", "tok") != nil || CloneAuth("https://github.com/a/b", "") != nil {
		t.Fatal("auth for a non-github URL or empty token")
	}
}

func TestReceive(t *testing.T) {
	body := []byte(`{"x":1}`)
	sig := "sha256=75e31067a7b58ac9207ca9b950a2104dbc31159e3dc2f2881ffd48f614e8786c"
	if !validSignature("s3cret", sig, body) {
		t.Fatal("vector rejected")
	}
	if validSignature("s3cret", "sha256=deadbeef", body) || validSignature("", sig, body) {
		t.Fatal("bad signature or empty secret accepted")
	}

	sign := func(b string) string {
		mac := hmac.New(sha256.New, []byte("k"))
		mac.Write([]byte(b))
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	if _, err := Receive("push", "sha256=00", []byte(`{}`), "k"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("unsigned: %v", err)
	}
	push := `{"ref":"refs/heads/main","after":"abc","commits":[{"added":["a"],"modified":["b"]},{"removed":["c"]}]}`
	ev, err := Receive("push", sign(push), []byte(push), "k")
	if err != nil || ev.Push == nil || ev.Push.After != "abc" || strings.Join(ev.Push.ChangedFiles(), ",") != "a,b,c" {
		t.Fatalf("push: %+v %v", ev.Push, err)
	}
	pr := `{"action":"opened","number":3,"pull_request":{"head":{"ref":"f","sha":"s1"},"base":{"ref":"main"}}}`
	if ev, err := Receive("pull_request", sign(pr), []byte(pr), "k"); err != nil || ev.PR.Number != 3 ||
		ev.PR.PullRequest.Head.SHA != "s1" {
		t.Fatalf("pr: %+v %v", ev, err)
	}
	if _, err := Receive("pull_request", sign(`{}`), []byte(`{}`), "k"); !errors.Is(err, ErrBadPayload) {
		t.Fatalf("pr without number: %v", err)
	}
	if ev, err := Receive("installation", sign(`{}`), []byte(`{}`), "k"); err != nil || ev.PR != nil || ev.Push != nil {
		t.Fatalf("ignored event: %+v %v", ev, err)
	}
}
