package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
)

// AllowTCP is POST /v1/allow-tcp: open (On) or close the node to tcp ports
// Lo..Hi from Src, for a live move's rsync helper. The helper publishes its
// ports, so Docker DNATs them and the packets are forwarded, not input: one
// rule in INPUT (a host-local listener) and one in DOCKER-USER (FORWARD
// without it) matching the original destination port. Both carry the
// "stackr-move" comment, and repeating a call changes nothing.
type AllowTCP struct {
	Src    string
	Lo, Hi int
	On     bool
}

const moveComment = "stackr-move"

func (s *Server) allowTCP(w http.ResponseWriter, r *http.Request) {
	b, ok := readBody(w, r)
	if !ok {
		return
	}
	var a AllowTCP
	if err := json.Unmarshal(b, &a); err != nil {
		fail(w, 400, err)
		return
	}
	if ip, err := netip.ParseAddr(a.Src); err != nil || !ip.Is4() {
		fail(w, 400, fmt.Errorf("src %q is not an IPv4 address", a.Src))
		return
	}
	if a.Lo < 1 || a.Hi > 65535 || a.Lo > a.Hi {
		fail(w, 400, fmt.Errorf("ports %d-%d out of range", a.Lo, a.Hi))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.allowRules(r.Context(), a); err != nil {
		fail(w, 500, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) allowRules(ctx context.Context, a AllowTCP) error {
	ports := fmt.Sprintf("%d:%d", a.Lo, a.Hi)
	fwd := "FORWARD"
	if _, err := s.cfg.Runner.Run(ctx, nil, "iptables", "-S", "DOCKER-USER"); err == nil {
		fwd = "DOCKER-USER"
	}
	rules := [][]string{
		{"INPUT", "-s", a.Src, "-p", "tcp", "--dport", ports, "-m", "comment", "--comment", moveComment, "-j", "ACCEPT"},
		{fwd, "-s", a.Src, "-p", "tcp", "-m", "conntrack", "--ctorigdstport", ports,
			"-m", "comment", "--comment", moveComment, "-j", "ACCEPT"},
	}
	ipt := func(op string, rest ...string) error {
		out, err := s.cfg.Runner.Run(ctx, nil, "iptables", append([]string{op}, rest...)...)
		if err != nil {
			return fmt.Errorf("iptables %s %v: %w: %s", op, rest, err, out)
		}
		return nil
	}
	for _, r := range rules {
		if a.On {
			if ipt("-C", r...) == nil {
				continue
			}
			if err := ipt("-I", append([]string{r[0], "1"}, r[1:]...)...); err != nil {
				return err
			}
			continue
		}
		for ipt("-D", r...) == nil {
		}
	}
	return nil
}
