package vip

import (
	"context"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	got := Render(
		map[string][]string{"10.0.0.9": {"10.0.0.3"}, "10.0.0.5": {"10.0.0.3"}},
		map[string][]string{"10.0.0.5": {"10.0.0.6", "10.0.0.7", "10.0.0.8"}, "10.0.1.2": {"10.0.1.3"}},
	)
	want := `*nat
:STACKR-VIP - [0:0]
:STKR-10.0.0.5 - [0:0]
:STKR-10.0.1.2 - [0:0]
:STKR-10.0.0.9 - [0:0]
-A STACKR-VIP -d 10.0.0.5/32 -j STKR-10.0.0.5
-A STKR-10.0.0.5 -m statistic --mode random --probability 0.33333 -j DNAT --to-destination 10.0.0.6
-A STKR-10.0.0.5 -m statistic --mode random --probability 0.50000 -j DNAT --to-destination 10.0.0.7
-A STKR-10.0.0.5 -j DNAT --to-destination 10.0.0.8
-A STACKR-VIP -d 10.0.1.2/32 -j STKR-10.0.1.2
-A STKR-10.0.1.2 -j DNAT --to-destination 10.0.1.3
-X STKR-10.0.0.9
COMMIT
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := Render(nil, nil); got != "*nat\n:STACKR-VIP - [0:0]\nCOMMIT\n" {
		t.Fatalf("empty: %q", got)
	}
}

func TestTable(t *testing.T) {
	var scripts []string
	var argvs []string
	jumped := false
	tb := New()
	tb.Restore = func(_ context.Context, s string) error {
		scripts = append(scripts, s)
		return nil
	}
	tb.Exec = func(_ context.Context, argv ...string) error {
		argvs = append(argvs, strings.Join(argv, " "))
		if argv[3] == "-C" && !jumped {
			return context.Canceled // any error: the jump is missing
		}
		if argv[3] == "-I" && argv[4] == "OUTPUT" {
			jumped = true
		}
		return nil
	}
	ctx := context.Background()
	if err := tb.Set(ctx, "10.0.0.5", []string{"10.0.0.6"}); err != nil {
		t.Fatal(err)
	}
	if err := tb.Set(ctx, "10.0.0.9", []string{"10.0.0.10"}); err != nil {
		t.Fatal(err)
	}
	if err := tb.Remove(ctx, "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
	last := scripts[len(scripts)-1]
	if strings.Contains(last, "-d 10.0.0.5/32") || !strings.Contains(last, "-X STKR-10.0.0.5") ||
		!strings.Contains(last, "10.0.0.10") {
		t.Fatalf("after remove:\n%s", last)
	}
	wantArgv := []string{
		"iptables -t nat -C PREROUTING -j STACKR-VIP",
		"iptables -t nat -I PREROUTING -j STACKR-VIP",
		"iptables -t nat -C OUTPUT -j STACKR-VIP",
		"iptables -t nat -I OUTPUT -j STACKR-VIP",
		"iptables -t nat -C PREROUTING -j STACKR-VIP",
		"iptables -t nat -C OUTPUT -j STACKR-VIP",
	}
	if strings.Join(argvs[:6], "\n") != strings.Join(wantArgv, "\n") {
		t.Fatalf("argv:\n%s", strings.Join(argvs, "\n"))
	}
	if err := tb.Set(ctx, "10.0.0.5", []string{"10.0.0.6\n-F"}); err == nil {
		t.Fatal("non-IP accepted")
	}
	if err := tb.Rebuild(ctx, map[string][]string{}); err != nil {
		t.Fatal(err)
	}
	if last := scripts[len(scripts)-1]; !strings.Contains(last, "-X STKR-10.0.0.9") {
		t.Fatalf("rebuild did not clear:\n%s", last)
	}
}
