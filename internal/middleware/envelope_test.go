package middleware_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

func envelope(t *testing.T, err error) (*httptest.ResponseRecorder, middleware.APIError) {
	t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec)
	if got := middleware.JSONErrors()(func(echo.Context) error { return err })(c); got != nil {
		t.Fatal(got)
	}
	var body middleware.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return rec, body
}

func TestErrorEnvelope(t *testing.T) {
	_, b := envelope(t, errors.Join(errs.Invalidf("name", "is required"), errs.Invalidf("level", "is wrong")))
	if b.Code != "invalid" || b.Field != "name" || len(b.Fields) != 2 || b.Fields[1].Field != "level" {
		t.Errorf("multi-field = %+v", b)
	}
	if _, b = envelope(t, errs.ErrNotFound); b.Code != "not_found" || b.Status != 404 {
		t.Errorf("not found = %+v", b)
	}
	if _, b = envelope(t, errs.NeedsApproval{What: "host access"}); b.Code != "needs_approval" || b.Status != 409 {
		t.Errorf("needs approval = %+v", b)
	}
	rec, b := envelope(t, errs.ErrBusy)
	if b.Code != "busy" || rec.Header().Get("Retry-After") == "" {
		t.Errorf("busy = %+v retry-after %q", b, rec.Header().Get("Retry-After"))
	}
}
