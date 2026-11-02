package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestPaged(t *testing.T) {
	get := func(q string) (echo.Context, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		return echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/?"+q, nil), rec), rec
	}
	all := []int{1, 2, 3, 4, 5}

	c, rec := get("limit=2")
	got, err := pageOf(c, all, nil)
	if err != nil || len(got) != 2 || got[1] != 2 {
		t.Fatalf("first page = %v %v", got, err)
	}
	cur := rec.Header().Get("X-Next-Cursor")
	if cur == "" {
		t.Fatal("no next cursor")
	}
	c, rec = get("limit=2&cursor=" + cur)
	got, _ = pageOf(c, all, nil)
	if len(got) != 2 || got[0] != 3 || rec.Header().Get("X-Next-Cursor") == "" {
		t.Errorf("second page = %v", got)
	}
	c, rec = get("limit=2&cursor=" + rec.Header().Get("X-Next-Cursor"))
	got, _ = pageOf(c, all, nil)
	if len(got) != 1 || got[0] != 5 || rec.Header().Get("X-Next-Cursor") != "" {
		t.Errorf("last page = %v", got)
	}
	c, _ = get("")
	if got, _ = pageOf(c, all, nil); len(got) != 5 {
		t.Errorf("no limit = %v, want the whole list", got)
	}
	for _, q := range []string{"limit=x", "limit=-1", "cursor=!!"} {
		if c, _ = get(q); func() bool { _, err := pageOf(c, all, nil); return err == nil }() {
			t.Errorf("%s accepted", q)
		}
	}
}
