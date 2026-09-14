package login

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	hamrauth "github.com/FyrmForge/hamr/pkg/auth"
	"github.com/labstack/echo/v4"
)

// failingSessions deletes nothing and says so, standing in for a store that is
// down or read-only at the moment someone logs out.
type failingSessions struct {
	session   *hamrauth.Session
	deleteErr error
	deleted   bool
}

func (f *failingSessions) Create(context.Context, *hamrauth.Session) error { return nil }

func (f *failingSessions) GetByToken(_ context.Context, token string) (*hamrauth.Session, error) {
	if f.session != nil && f.session.Token == token {
		return f.session, nil
	}
	return nil, nil
}

func (f *failingSessions) Delete(context.Context, string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = true
	return nil
}

func (f *failingSessions) DeleteBySubjectID(context.Context, string) error { return nil }

// A logout that cannot delete the session server-side must not clear the
// cookie or claim success: the session row stays valid until it expires, so
// telling the user they are out would leave a live token on a signed-out
// account.
func TestLogoutKeepsSessionWhenDeleteFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deleteErr error
		wantPath  string
		wantGone  bool
	}{
		{"delete fails", errors.New("database is locked"), "/", false},
		{"delete works", nil, "/login", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &failingSessions{
				session: &hamrauth.Session{
					ID:        "sess-1",
					SubjectID: "user-1",
					Token:     "tok-1",
					ExpiresAt: time.Now().Add(time.Hour),
					CreatedAt: time.Now(),
				},
				deleteErr: tc.deleteErr,
			}
			sm := hamrauth.NewSessionManager(store)
			h := NewHandler(nil, sm)

			req := httptest.NewRequest(http.MethodPost, "/logout", nil)
			req.AddCookie(&http.Cookie{Name: sm.CookieName(), Value: "tok-1"})
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(req, rec)

			if err := h.Logout(c); err != nil {
				t.Fatalf("Logout: %v", err)
			}
			if got := rec.Header().Get("HX-Redirect") + rec.Header().Get("Location"); got != tc.wantPath {
				t.Errorf("redirect = %q, want %q", got, tc.wantPath)
			}
			if store.deleted != tc.wantGone {
				t.Errorf("session deleted = %v, want %v", store.deleted, tc.wantGone)
			}
			if cleared := clearsCookie(rec, sm.CookieName()); cleared != tc.wantGone {
				t.Errorf("cookie cleared = %v, want %v", cleared, tc.wantGone)
			}
		})
	}
}

// clearsCookie reports whether the response expires the session cookie.
func clearsCookie(rec *httptest.ResponseRecorder, name string) bool {
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == name && (ck.MaxAge < 0 || ck.Value == "") {
			return true
		}
	}
	return false
}
