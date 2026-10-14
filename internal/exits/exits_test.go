package exits

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolve(t *testing.T) {
	l := Levels{Tile: []string{"t"}, Env: []string{"e"}, Stack: []string{"s"}, Org: []string{"o"}}
	def := []string{"d"}
	cases := []struct {
		name string
		in   Levels
		src  string
		want []string
	}{
		{"tile first", l, SourceTile, []string{"t"}},
		{"env", Levels{Env: l.Env, Stack: l.Stack, Org: l.Org}, SourceEnv, []string{"e"}},
		{"stack", Levels{Stack: l.Stack, Org: l.Org}, SourceStack, []string{"s"}},
		{"org", Levels{Org: l.Org}, SourceOrg, []string{"o"}},
		{"node beats default", Levels{Node: []string{"n"}}, SourceNode, []string{"n"}},
		{"node loses to org", Levels{Org: l.Org, Node: []string{"n"}}, SourceOrg, []string{"o"}},
		{"default", Levels{}, SourceDefault, []string{"d"}},
		{"empty slice inherits", Levels{Tile: []string{}, Env: []string{""}, Org: l.Org}, SourceOrg, []string{"o"}},
		{"dedupe keeps order", Levels{Env: []string{"b", "a", "b", "c", "a"}}, SourceEnv, []string{"b", "a", "c"}},
	}
	for _, c := range cases {
		got := Resolve(c.in, def)
		if got.Source != c.src || !reflect.DeepEqual(got.Nodes, c.want) {
			t.Errorf("%s: %+v, want %s %v", c.name, got, c.src, c.want)
		}
	}
	none := Resolve(Levels{}, nil)
	if none.Source != SourceNone || len(none.Nodes) != 0 || none.Routed("n1") {
		t.Errorf("none = %+v", none)
	}
	// The own node is not implied: a ladder without it never falls back to it.
	r := Resolve(Levels{Env: []string{"x", "y"}}, nil)
	if !r.Routed("n1") || len(r.Nodes) != 2 {
		t.Errorf("routed = %+v", r)
	}
	if own := Resolve(Levels{Env: []string{"n1"}}, nil); own.Routed("n1") {
		t.Error("own node only ladder needs no machinery")
	}
}

func TestIface(t *testing.T) {
	a, b := Iface("node-a"), Iface("node-b")
	if a == b || len(a) > 15 || !strings.HasPrefix(a, "wgx-") || a != Iface("node-a") {
		t.Errorf("iface %q %q", a, b)
	}
}

func joined(steps []Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, strings.Join(s.Argv, " "))
	}
	return out
}

func has(t *testing.T, lines []string, want string) {
	t.Helper()
	for _, l := range lines {
		if l == want {
			return
		}
	}
	t.Errorf("missing %q in\n%s", want, strings.Join(lines, "\n"))
}

func link() Link {
	return Link{Iface: "wgx-b", KeyFile: "/k", Port: 51821, PeerKey: "PB", Endpoint: "192.0.2.11:51821",
		Addr: "10.251.0.1/30", Table: 101, Sources: []string{"10.214.1.0/24"}}
}

func TestLinkUp(t *testing.T) {
	s, err := link().Up()
	if err != nil {
		t.Fatal(err)
	}
	l := joined(s)
	for _, w := range []string{
		"ip link add wgx-b type wireguard",
		"wg set wgx-b listen-port 51821 private-key /k peer PB endpoint 192.0.2.11:51821 allowed-ips 0.0.0.0/0 persistent-keepalive 25",
		"ip addr add 10.251.0.1/30 dev wgx-b",
		"ip link set wgx-b mtu 1420 up",
		"ip route replace default dev wgx-b table 101",
		"iptables -t nat -I STACKR-NAT 1 -o wgx-b -j ACCEPT",
		"iptables -t raw -I STACKR-RAW 1 -i wgx-b -d 10.214.1.0/24 -j ACCEPT",
		"iptables -t filter -I STACKR-XN 1 -i wgx-b -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"iptables -t filter -A STACKR-XN -i wgx-b -j DROP",
	} {
		has(t, l, w)
	}
	// iptables steps are idempotent through -C.
	n := 0
	for _, st := range s {
		if st.Check != nil {
			n++
			if st.Check[3] != "-C" {
				t.Errorf("check = %v", st.Check)
			}
		}
	}
	if n != 4 {
		t.Errorf("%d checked steps", n)
	}
	bad := []Link{
		func() Link { l := link(); l.Sources = nil; return l }(),
		func() Link { l := link(); l.Sources = []string{"nope"}; return l }(),
		func() Link { l := link(); l.Endpoint = "1.2.3.4"; return l }(),
		func() Link { l := link(); l.Addr = "10.251.0.1"; return l }(),
		func() Link { l := link(); l.Iface = "wgx-way-too-long-name"; return l }(),
		func() Link { l := link(); l.Table = 0; return l }(),
	}
	for i, b := range bad {
		if _, err := b.Up(); err == nil {
			t.Errorf("bad link %d accepted", i)
		}
	}
}

func TestLinkDown(t *testing.T) {
	for _, s := range link().Down() {
		if !s.IgnoreErr {
			t.Errorf("%v must tolerate failure", s.Argv)
		}
	}
	has(t, joined(link().Down()), "ip link del wgx-b")
}

func TestSubjectRules(t *testing.T) {
	s := Subject{Sources: []string{"10.214.1.0/24", "10.214.3.0/24"}, PrefBase: 1000}
	r, err := s.Route(101)
	if err != nil {
		t.Fatal(err)
	}
	l := joined(r)
	has(t, l, "ip rule add pref 1000 from 10.214.1.0/24 lookup main suppress_prefixlength 0")
	has(t, l, "ip rule add pref 1001 from 10.214.1.0/24 lookup 101")
	has(t, l, "ip rule add pref 1002 from 10.214.3.0/24 lookup main suppress_prefixlength 0")
	has(t, l, "ip rule add pref 1003 from 10.214.3.0/24 lookup 101")
	// Deletes come before adds so a re-route never stacks duplicate rules.
	if !strings.HasPrefix(l[0], "ip rule del pref") {
		t.Errorf("first step %q", l[0])
	}
	b, _ := s.Blackhole()
	bl := joined(b)
	has(t, bl, "ip rule add pref 1001 from 10.214.1.0/24 blackhole")
	has(t, bl, "ip rule add pref 1000 from 10.214.1.0/24 lookup main suppress_prefixlength 0")
	for _, c := range s.Clear() {
		if !c.IgnoreErr {
			t.Error("clear must tolerate missing rules")
		}
	}
	if _, err := (Subject{Sources: s.Sources}).Route(101); err == nil {
		t.Error("missing pref base accepted")
	}
	if _, err := s.Route(0); err == nil {
		t.Error("table 0 accepted")
	}
}

func serve() Serve {
	return Serve{Iface: "wgx-a", KeyFile: "/k", Port: 51821, PeerKey: "PA", Endpoint: "192.0.2.10:51821",
		Addr: "10.251.0.2/30", PeerAddr: "10.251.0.1/32", Subnets: []string{"10.214.1.0/24"},
		Uplink: "eth0", Table: 151, Mark: 0x51, Pref: 1000}
}

func TestServeUp(t *testing.T) {
	v := serve()
	if got := v.AllowedIPs(); got != "10.214.1.0/24,10.251.0.1/32" {
		t.Errorf("allowed = %s", got)
	}
	s, err := v.Up()
	if err != nil {
		t.Fatal(err)
	}
	l := joined(s)
	for _, w := range []string{
		"wg set wgx-a listen-port 51821 private-key /k peer PA endpoint 192.0.2.10:51821 allowed-ips 10.214.1.0/24,10.251.0.1/32",
		"iptables -t mangle -A STACKR-XM-wgx-a -i wgx-a -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x51",
		"iptables -t mangle -A STACKR-XM-wgx-a -m connmark --mark 0x51 -j CONNMARK --restore-mark",
		"iptables -t nat -A STACKR-XT-wgx-a -s 10.214.1.0/24 -o eth0 -j MASQUERADE",
		"iptables -t filter -A STACKR-XF-wgx-a -o wgx-a -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"iptables -t filter -A STACKR-XF-wgx-a -i wgx-a -o eth0 -j ACCEPT",
		"iptables -t filter -A STACKR-XF-wgx-a -i wgx-a -j DROP",
		"iptables -t mangle -I PREROUTING 1 -j STACKR-XM-wgx-a",
		"iptables -t nat -I POSTROUTING 1 -j STACKR-XT-wgx-a",
		"iptables -t filter -I DOCKER-USER 1 -j STACKR-XF-wgx-a",
		"iptables -t filter -A INPUT -i wgx-a -j DROP",
		"ip route replace 10.214.1.0/24 dev wgx-a table 151",
		"ip rule add pref 1000 fwmark 0x51 lookup 151",
	} {
		has(t, l, w)
	}
	// Every refused range drops before the uplink accept.
	acc := -1
	drops := map[string]int{}
	for i, line := range l {
		if line == "iptables -t filter -A STACKR-XF-wgx-a -i wgx-a -o eth0 -j ACCEPT" {
			acc = i
		}
		for _, r := range Refused {
			if line == "iptables -t filter -A STACKR-XF-wgx-a -i wgx-a -d "+r+" -j DROP" {
				drops[r] = i
			}
		}
	}
	if len(drops) != 5 || len(Refused) != 5 {
		t.Fatalf("drops = %v", drops)
	}
	for r, i := range drops {
		if i > acc {
			t.Errorf("%s dropped after the accept", r)
		}
	}
	for _, r := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"} {
		if _, ok := drops[r]; !ok {
			t.Errorf("%s not refused", r)
		}
	}
	// Chains are flushed before they are refilled.
	f, a := -1, -1
	for i, line := range l {
		if line == "iptables -t mangle -F STACKR-XM-wgx-a" {
			f = i
		}
		if strings.HasPrefix(line, "iptables -t mangle -A STACKR-XM-wgx-a") && a < 0 {
			a = i
		}
	}
	if f < 0 || f > a {
		t.Errorf("flush %d, first append %d", f, a)
	}
	for _, b := range []func(*Serve){
		func(v *Serve) { v.Subnets = nil },
		func(v *Serve) { v.Subnets = []string{"10.0.0.0"} },
		func(v *Serve) { v.PeerAddr = "x" },
		func(v *Serve) { v.Mark = 0 },
		func(v *Serve) { v.Uplink = "" },
		func(v *Serve) { v.Endpoint = "nohost" },
	} {
		v := serve()
		b(&v)
		if _, err := v.Up(); err == nil {
			t.Errorf("bad serve accepted: %+v", v)
		}
	}
	v.Endpoint = ""
	if s, _ := v.Up(); strings.Contains(strings.Join(joined(s), "\n"), "endpoint") {
		t.Error("empty endpoint still passed to wg")
	}
	for _, s := range serve().Down() {
		if !s.IgnoreErr {
			t.Errorf("%v must tolerate failure", s.Argv)
		}
	}
}

func TestApply(t *testing.T) {
	var ran []string
	r := RunnerFunc(func(_ context.Context, a []string) error {
		j := strings.Join(a, " ")
		ran = append(ran, j)
		if j == "chk-done" {
			return nil
		}
		if strings.HasPrefix(j, "chk") || j == "boom" {
			return errors.New("exit 1")
		}
		return nil
	})
	err := Apply(context.Background(), r, []Step{
		{Argv: []string{"skipped"}, Check: []string{"chk-done"}},
		{Argv: []string{"added"}, Check: []string{"chk-missing"}},
		{Argv: []string{"boom"}, IgnoreErr: true},
		{Argv: []string{"boom"}},
		{Argv: []string{"never"}},
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	want := []string{"chk-done", "chk-missing", "added", "boom", "boom"}
	if !reflect.DeepEqual(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
}

// harness drives a ladder with a scripted set of live rungs.
type harness struct {
	up    map[string]bool
	cmds  []string
	pings []string
	l     *Ladder
	t0    time.Time
	seen  []State
}

func newHarness(rungs ...Rung) *harness {
	h := &harness{up: map[string]bool{}, t0: time.Unix(1000, 0)}
	for _, r := range rungs {
		h.up[r.NodeID] = true
	}
	h.l = NewLadder(Subject{Sources: []string{"10.214.1.0/24"}, PrefBase: 1000}, rungs,
		RunnerFunc(func(_ context.Context, a []string) error { h.cmds = append(h.cmds, strings.Join(a, " ")); return nil }),
		PingerFunc(func(_ context.Context, r Rung) error {
			h.pings = append(h.pings, r.NodeID)
			if h.up[r.NodeID] {
				return nil
			}
			return errors.New("timeout")
		}))
	h.l.Now = func() time.Time { return h.t0 }
	h.l.OnChange = func(s State) { h.seen = append(h.seen, s) }
	return h
}

func (h *harness) tick(t *testing.T) State {
	t.Helper()
	h.cmds, h.pings = nil, nil
	s, err := h.l.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (h *harness) routed(table string) bool {
	for _, c := range h.cmds {
		if strings.HasSuffix(c, "lookup "+table) {
			return true
		}
	}
	return false
}

func (h *harness) blackholed() bool {
	for _, c := range h.cmds {
		if strings.HasSuffix(c, "blackhole") {
			return true
		}
	}
	return false
}

var (
	rb  = Rung{NodeID: "b", Iface: "wgx-b", Table: 101, Target: "1.1.1.1"}
	rc  = Rung{NodeID: "c", Iface: "wgx-c", Table: 102, Target: "1.1.1.1"}
	own = Rung{NodeID: "a", Own: true, Target: "1.1.1.1"}
)

func TestLadderStartAndSteady(t *testing.T) {
	h := newHarness(rb, rc)
	s := h.tick(t)
	if s.Active != 0 || s.NodeID != "b" || s.Down || !h.routed("101") {
		t.Fatalf("start = %+v cmds %v", s, h.cmds)
	}
	// Healthy and steady: only the live rung is probed, nothing reapplied,
	// no change event.
	for i := 0; i < 5; i++ {
		h.tick(t)
		if len(h.cmds) != 0 || !reflect.DeepEqual(h.pings, []string{"b"}) {
			t.Fatalf("steady tick cmds %v pings %v", h.cmds, h.pings)
		}
	}
	if len(h.seen) != 1 {
		t.Errorf("%d change events", len(h.seen))
	}
}

func TestLadderFailoverAndRecovery(t *testing.T) {
	h := newHarness(rb, rc, own)
	h.tick(t)
	h.up["b"] = false
	// Two failures are tolerated (FailAfter 3).
	for i := 0; i < 2; i++ {
		if s := h.tick(t); s.Active != 0 || len(h.cmds) != 0 {
			t.Fatalf("flap %d switched: %+v", i, s)
		}
	}
	s := h.tick(t)
	if s.Active != 1 || s.NodeID != "c" || !h.routed("102") {
		t.Fatalf("third failure: %+v cmds %v", s, h.cmds)
	}
	// c dies, then own uplink takes over: rules are cleared, not installed.
	h.up["c"] = false
	for i := 0; i < 3; i++ {
		s = h.tick(t)
	}
	if s.Active != 2 || s.NodeID != "a" || h.routed("101") || h.routed("102") || h.blackholed() {
		t.Fatalf("own rung: %+v cmds %v", s, h.cmds)
	}
	// b returns: needs RecoverAfter (2) good probes in a row, then goes back up.
	h.up["b"] = true
	if s = h.tick(t); s.Active != 2 {
		t.Fatalf("recovered after one probe: %+v", s)
	}
	h.t0 = h.t0.Add(time.Minute)
	s = h.tick(t)
	if s.Active != 0 || !h.routed("101") || !s.Since.Equal(h.t0) {
		t.Fatalf("recovery: %+v cmds %v", s, h.cmds)
	}
	// b blips once while good: the streak resets, no switch.
	h.up["b"] = false
	if s = h.tick(t); s.Active != 0 {
		t.Fatalf("one blip switched: %+v", s)
	}
}

func TestLadderExhausted(t *testing.T) {
	h := newHarness(rb, rc)
	h.up["b"], h.up["c"] = false, false
	// First pass: nothing answers, the subject is blackholed at once rather
	// than leaking out the own uplink.
	s := h.tick(t)
	if !s.Down || s.Active != -1 || !h.blackholed() {
		t.Fatalf("exhausted: %+v cmds %v", s, h.cmds)
	}
	if !strings.Contains(s.Reason, "b: timeout") || !strings.Contains(s.Reason, "c: timeout") {
		t.Errorf("reason = %q", s.Reason)
	}
	// Still down: all rungs probed, rules left alone, one event only.
	h.tick(t)
	if len(h.cmds) != 0 || len(h.pings) != 2 || len(h.seen) != 1 {
		t.Errorf("down tick cmds %v pings %v events %d", h.cmds, h.pings, len(h.seen))
	}
	// The lower rung comes back first: one good probe is enough from down.
	h.up["c"] = true
	s = h.tick(t)
	if s.Down || s.NodeID != "c" || !h.routed("102") {
		t.Fatalf("back from down: %+v cmds %v", s, h.cmds)
	}
	// Failing mid ladder with nothing left reports down with a reason.
	h.up["c"] = false
	for i := 0; i < 3; i++ {
		s = h.tick(t)
	}
	if !s.Down || !h.blackholed() || s.Reason == "" {
		t.Fatalf("second exhaustion: %+v", s)
	}
}

func TestLadderEmptyAndRuleFailure(t *testing.T) {
	h := newHarness()
	s := h.tick(t)
	if !s.Down || s.Reason != "no exits listed" {
		t.Errorf("empty = %+v", s)
	}

	// A rule failure keeps the old state, so the next tick retries.
	h = newHarness(rb)
	fail := true
	h.l.Cmd = RunnerFunc(func(_ context.Context, a []string) error {
		if fail && a[1] == "rule" && a[2] == "add" {
			return errors.New("ip failed")
		}
		return nil
	})
	if _, err := h.l.Tick(context.Background()); err == nil {
		t.Fatal("rule failure swallowed")
	}
	if st := h.l.State(); st.Active != -1 || len(h.seen) != 0 {
		t.Errorf("state moved on failure: %+v", st)
	}
	fail = false
	if s, err := h.l.Tick(context.Background()); err != nil || s.Active != 0 {
		t.Errorf("retry = %+v, %v", s, err)
	}
}

func TestLadderLoop(t *testing.T) {
	h := newHarness(rb)
	ctx, cancel := context.WithCancel(context.Background())
	h.l.OnChange = func(State) { cancel() }
	done := make(chan struct{})
	go func() { h.l.Loop(ctx, time.Millisecond, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop")
	}
	if h.l.State().Active != 0 {
		t.Errorf("state = %+v", h.l.State())
	}
}
