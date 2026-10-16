package v1

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// An IPv6 client is counted by its /64; IPv4 and other /64s keep their own.
func TestLimiterKeysIPv6ByPrefix(t *testing.T) {
	l := newTryLimiter(2)
	for i, ip := range []string{"2001:db8:1:2::1", "2001:db8:1:2:aaaa::2"} {
		if ok, _ := l.allow(ip); !ok {
			t.Fatalf("try %d refused", i)
		}
	}
	if ok, _ := l.allow("2001:db8:1:2:ffff:ffff:ffff:ffff"); ok {
		t.Fatal("a third address of the same /64 got a fresh budget")
	}
	if ok, _ := l.allow("2001:db8:1:3::1"); !ok {
		t.Fatal("another /64 was counted with the first")
	}
	if ok, _ := l.allow("192.0.2.1"); !ok {
		t.Fatal("an IPv4 client was counted with the IPv6 ones")
	}
}

// A limiter full of live keys admits a new client by dropping the oldest.
func TestLimiterFullAdmitsNewKey(t *testing.T) {
	l := newTryLimiter(2)
	for i := range joinMaxKeys {
		l.allow(fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&255, i&255))
	}
	if ok, _ := l.allow("192.0.2.1"); !ok {
		t.Fatal("a new client was refused while the limiter was full")
	}
	if len(l.m) != joinMaxKeys {
		t.Fatalf("keys = %d, want %d", len(l.m), joinMaxKeys)
	}
}

// Join and recover do not share a budget.
func TestJoinBudgetDoesNotBlockRecover(t *testing.T) {
	ip := "2001:db8:9:9::1"
	for range joinPerWindow + 1 {
		joinTries.allow(ip)
	}
	if ok, _ := joinTries.allow(ip); ok {
		t.Fatal("the join budget did not run out")
	}
	if ok, _ := nodeTries.allow(ip); !ok {
		t.Fatal("filling the join budget blocked a worker's recovery")
	}
}

// X-Forwarded-Proto counts only from a trusted proxy.
func TestIsHTTPSTrustsTheHeaderOnlyFromTheProxy(t *testing.T) {
	e := echo.New()
	e.IPExtractor = echo.ExtractIPFromXFFHeader(echo.TrustIPRange(mustCIDR(t, "172.17.0.0/16")))
	ctx := func(remote string, hdr map[string]string) echo.Context {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = remote
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return e.NewContext(r, httptest.NewRecorder())
	}
	if !isHTTPS(ctx("172.17.0.2:5555", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.9"})) {
		t.Error("Caddy's https was not believed")
	}
	if isHTTPS(ctx("172.17.0.2:5555", map[string]string{"X-Forwarded-For": "203.0.113.9"})) {
		t.Error("no header, yet https")
	}
	if isHTTPS(ctx("203.0.113.9:5555", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "198.51.100.1"})) {
		t.Error("a direct client set X-Forwarded-Proto and got https")
	}
	if isHTTPS(ctx("203.0.113.9:5555", map[string]string{"X-Forwarded-Proto": "https"})) {
		t.Error("a direct client set X-Forwarded-Proto and got https")
	}
}
