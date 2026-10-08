package vip

import (
	"context"
	"net/netip"
	"strings"
	"testing"
)

var testBase = Base{
	Range:     netip.MustParsePrefix("10.213.0.0/16"),
	Resolvers: []string{"192.168.1.1"},
	ProxyIP:   "172.17.0.2",
	Front:     []string{"192.168.1.100", "173.245.48.0/20"},
	PanelBind: "172.17.0.1",
	PanelPort: 8080,
}

func TestRenderFilter(t *testing.T) {
	got := Render(nil, map[string]Entry{
		"10.213.1.2": {Replicas: []string{"10.213.1.3", "10.213.1.4"}, Lan: []string{"lan:192.168.1.10:8123"}},
		"10.213.2.2": {Replicas: []string{"10.213.2.3"}, Lan: []string{"all"}},
		"10.213.3.2": {Replicas: []string{"10.213.3.3"}},
	}, testBase)
	i := strings.Index(got, "*filter")
	if i < 0 {
		t.Fatalf("no filter table:\n%s", got)
	}
	want := `*filter
:STACKR-FWD - [0:0]
:STACKR-IN - [0:0]
-A STACKR-FWD -m conntrack ! --ctstate NEW -j RETURN
-A STACKR-FWD ! -s 10.213.0.0/16 -j RETURN
-A STACKR-FWD -d 10.213.0.0/16 -j RETURN
-A STACKR-FWD -d 192.168.1.1/32 -p udp --dport 53 -j RETURN
-A STACKR-FWD -d 192.168.1.1/32 -p tcp --dport 53 -j RETURN
-A STACKR-FWD -d 172.17.0.2/32 -p tcp -m multiport --dports 80,443 -j ACCEPT
-A STACKR-FWD -d 192.168.1.100/32 -p tcp -m multiport --dports 80,443 -j RETURN
-A STACKR-FWD -d 173.245.48.0/20 -p tcp -m multiport --dports 80,443 -j RETURN
-A STACKR-FWD -s 10.213.1.3/32 -d 192.168.1.10/32 -p tcp --dport 8123 -j RETURN
-A STACKR-FWD -s 10.213.1.3/32 -d 192.168.1.10/32 -p udp --dport 8123 -j RETURN
-A STACKR-FWD -s 10.213.1.4/32 -d 192.168.1.10/32 -p tcp --dport 8123 -j RETURN
-A STACKR-FWD -s 10.213.1.4/32 -d 192.168.1.10/32 -p udp --dport 8123 -j RETURN
-A STACKR-FWD -s 10.213.2.3/32 -j RETURN
-A STACKR-FWD -d 10.0.0.0/8 -j DROP
-A STACKR-FWD -d 172.16.0.0/12 -j DROP
-A STACKR-FWD -d 192.168.0.0/16 -j DROP
-A STACKR-FWD -d 169.254.0.0/16 -j DROP
-A STACKR-FWD -d 100.64.0.0/10 -j DROP
-A STACKR-IN -m conntrack ! --ctstate NEW -j RETURN
-A STACKR-IN -i lo -j RETURN
-A STACKR-IN -s 10.213.0.0/16 -d 192.168.1.1/32 -p udp --dport 53 -j RETURN
-A STACKR-IN -s 10.213.0.0/16 -d 192.168.1.1/32 -p tcp --dport 53 -j RETURN
-A STACKR-IN -d 172.17.0.1/32 -p tcp --dport 8080 ! -s 172.17.0.2/32 -j DROP
-A STACKR-IN -s 10.213.1.3/32 -d 192.168.1.10/32 -p tcp --dport 8123 -j RETURN
-A STACKR-IN -s 10.213.1.3/32 -d 192.168.1.10/32 -p udp --dport 8123 -j RETURN
-A STACKR-IN -s 10.213.1.4/32 -d 192.168.1.10/32 -p tcp --dport 8123 -j RETURN
-A STACKR-IN -s 10.213.1.4/32 -d 192.168.1.10/32 -p udp --dport 8123 -j RETURN
-A STACKR-IN -s 10.213.2.3/32 -j RETURN
-A STACKR-IN -s 10.213.0.0/16 -j DROP
COMMIT
`
	if got[i:] != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got[i:], want)
	}
	if strings.Contains(Render(nil, nil, Base{}), "filter") {
		t.Fatal("filter rendered without a base")
	}
}

func TestFilterTable(t *testing.T) {
	var scripts, argvs []string
	top := "" // what `iptables -S <hook> 1` prints
	tb := New()
	tb.Restore = func(_ context.Context, s string) error { scripts = append(scripts, s); return nil }
	deleted := map[string]bool{} // -D succeeds once per jump, then fails like iptables
	tb.Exec = func(_ context.Context, argv ...string) error {
		j := strings.Join(argv, " ")
		argvs = append(argvs, j)
		if argv[1] == "-D" {
			if deleted[j] {
				return context.Canceled
			}
			deleted[j] = true
		}
		return nil
	}
	tb.Read = func(_ context.Context, argv ...string) (string, error) { return top, nil }
	ctx := context.Background()

	if err := tb.Rebuild(ctx, nil, testBase); err != nil {
		t.Fatal(err)
	}
	if err := tb.Set(ctx, "10.213.1.2", []string{"10.213.1.3"}, []string{"lan:all"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scripts[1], "-s 10.213.1.3/32 -j RETURN") {
		t.Fatalf("lan rule missing:\n%s", scripts[1])
	}
	// A replica change re-renders the lan rules.
	if err := tb.Set(ctx, "10.213.1.2", []string{"10.213.1.5"}, []string{"lan:all"}); err != nil {
		t.Fatal(err)
	}
	if s := scripts[2]; strings.Contains(s, "10.213.1.3") || !strings.Contains(s, "-s 10.213.1.5/32 -j RETURN") {
		t.Fatalf("replica change:\n%s", s)
	}
	// Remove drops them.
	if err := tb.Remove(ctx, "10.213.1.2"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(scripts[3], "10.213.1.5/32 -j RETURN") {
		t.Fatalf("remove kept lan rules:\n%s", scripts[3])
	}
	// Rebuild and Merge carry lan.
	if err := tb.Rebuild(ctx, map[string]Entry{"10.213.1.2": {Replicas: []string{"10.213.1.6"}, Lan: []string{"lan:10.0.0.0/24"}}}, testBase); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scripts[4], "-s 10.213.1.6/32 -d 10.0.0.0/24 -j RETURN") {
		t.Fatalf("rebuild:\n%s", scripts[4])
	}
	clear(deleted)
	if err := tb.Merge(ctx, map[string]Entry{"10.213.2.2": {Replicas: []string{"10.213.2.3"}}}, testBase); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scripts[5], "10.213.1.6/32 -d 10.0.0.0/24") {
		t.Fatalf("merge dropped lan:\n%s", scripts[5])
	}

	// Jumps are asserted at position 1: missing or displaced is re-inserted.
	want := "iptables -D FORWARD -j STACKR-FWD\niptables -D FORWARD -j STACKR-FWD\niptables -I FORWARD 1 -j STACKR-FWD\n" +
		"iptables -D INPUT -j STACKR-IN\niptables -D INPUT -j STACKR-IN\niptables -I INPUT 1 -j STACKR-IN"
	if got := strings.Join(argvs[len(argvs)-6:], "\n"); got != want {
		t.Fatalf("jumps:\n%s", got)
	}
	argvs = nil
	top = "-A FORWARD -j STACKR-FWD\n"
	if err := tb.Rebuild(ctx, nil, testBase); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argvs, "\n"), "FORWARD 1") {
		t.Fatalf("forward jump re-inserted though on top:\n%s", strings.Join(argvs, "\n"))
	}
}

func TestFilterRefusesBadAddress(t *testing.T) {
	tb := New()
	tb.Restore = func(context.Context, string) error { t.Fatal("bad input reached the script"); return nil }
	tb.Exec = func(context.Context, ...string) error { return nil }
	ctx := context.Background()
	for _, lan := range []string{"192.168.1.1\n-F", "lan:300.1.1.1", "lan:10.0.0.1:99999", "lan:::1", "lan:10.0.0.0/33"} {
		if err := tb.Set(ctx, "10.213.1.2", []string{"10.213.1.3"}, []string{lan}); err == nil {
			t.Errorf("lan %q accepted", lan)
		}
	}
	for _, b := range []Base{
		{Range: testBase.Range, Resolvers: []string{"1.1.1.1\n-F"}},
		{Range: testBase.Range, Front: []string{"x"}},
		{Range: testBase.Range, ProxyIP: "::1"},
		{Range: testBase.Range, PanelBind: "bad"},
	} {
		if err := tb.Rebuild(ctx, nil, b); err == nil {
			t.Errorf("base %+v accepted", b)
		}
	}
}

// No grant opens the panel: its rule sits before every lan return in INPUT,
// and a lan grant lands in INPUT too.
func TestFilterInputLan(t *testing.T) {
	got := Render(nil, map[string]Entry{"10.213.1.2": {Replicas: []string{"10.213.1.3"}, Lan: []string{"all"}}}, testBase)
	panel := strings.Index(got, "-A STACKR-IN -d 172.17.0.1/32 -p tcp --dport 8080")
	lan := strings.Index(got, "-A STACKR-IN -s 10.213.1.3/32 -j RETURN")
	drop := strings.Index(got, "-A STACKR-IN -s 10.213.0.0/16 -j DROP")
	if panel < 0 || lan < panel || drop < lan {
		t.Fatalf("order panel=%d lan=%d drop=%d:\n%s", panel, lan, drop, got)
	}
}
