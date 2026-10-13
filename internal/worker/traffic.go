package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
)

// Traffic is GET /v1/traffic: this node's conntrack lines that touch one of
// its own tile containers, and those containers' IPs by tile id. The
// controller runs the counters (leaf/traffic); lines with no local tile at
// either end (traffic this node only forwards for others) never leave.
type Traffic struct {
	Tiles map[string]string `json:"tiles"` // container IP -> tile id
	Lines string            `json:"lines"`
	// Gateways of this node's stackr networks: the host side, where a
	// host-network Caddy (an ingress worker's) reaches a tile from.
	Gateways []string `json:"gateways,omitempty"`
}

// Labels the controller puts on tile containers (docker.LabelManaged,
// tile.LabelTile); the worker cannot import the service tree.
const (
	labelManaged = "stackr.managed"
	labelTile    = "stackr.tile"
)

const conntrackPath = "/proc/net/nf_conntrack"

// acctKnob is a var for the test.
var acctKnob = "/proc/sys/net/netfilter/nf_conntrack_acct"

func (s *Server) traffic(w http.ResponseWriter, r *http.Request) {
	tiles, gws, err := s.localTiles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	raw, err := ReadConntrack(r.Context(), s.conntrack())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Traffic{Tiles: tiles, Lines: string(localLines(raw, tiles)), Gateways: gws})
}

func (s *Server) conntrack() string {
	if s.cfg.Conntrack != "" {
		return s.cfg.Conntrack
	}
	return conntrackPath
}

// localTiles maps every IP of this node's running tile containers to its
// tile id, and lists the gateways of its stackr networks, from the local
// Docker.
func (s *Server) localTiles(ctx context.Context) (map[string]string, []string, error) {
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.cfg.DockerSock)
		}}}
	defer hc.CloseIdleConnections()
	get := func(path, filters string, v any) error {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://local"+path+"?filters="+url.QueryEscape(filters), nil)
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return json.NewDecoder(resp.Body).Decode(v)
	}
	managed := `"label":["` + labelManaged + `=true"]`
	var cs []struct {
		Labels          map[string]string
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	if err := get("/containers/json", `{`+managed+`,"status":["running"]}`, &cs); err != nil {
		return nil, nil, err
	}
	var ns []struct {
		IPAM struct {
			Config []struct{ Subnet, Gateway string }
		}
	}
	if err := get("/networks", `{`+managed+`}`, &ns); err != nil {
		return nil, nil, err
	}
	var gws []string
	for _, n := range ns {
		for _, c := range n.IPAM.Config {
			// An unset gateway is Docker's default, the subnet's first host.
			if p, err := netip.ParsePrefix(c.Subnet); c.Gateway == "" && err == nil {
				c.Gateway = p.Masked().Addr().Next().String()
			}
			if c.Gateway != "" {
				gws = append(gws, c.Gateway)
			}
		}
	}
	out := map[string]string{}
	for _, c := range cs {
		id := c.Labels[labelTile]
		if id == "" {
			continue
		}
		for _, n := range c.NetworkSettings.Networks {
			if n.IPAddress != "" {
				out[n.IPAddress] = id
			}
		}
	}
	return out, gws, nil
}

// ReadConntrack is the table: the procfs file, conntrack -L, or the kernel's
// netlink dump (a distro kernel without the procfs file and no conntrack
// tool, e.g. Ubuntu 26.04), all in conntrack -L's line format.
func ReadConntrack(ctx context.Context, path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return data, err
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if out, xerr := exec.CommandContext(ctx, "conntrack", "-L").Output(); xerr == nil {
		return out, nil
	}
	flows, nerr := netlink.ConntrackTableList(netlink.ConntrackTable, netlink.FAMILY_V4)
	if nerr != nil {
		return nil, err
	}
	var out bytes.Buffer
	for _, f := range flows {
		out.WriteString(f.String())
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// localLines keeps the lines whose original src or dst is a local tile IP.
func localLines(raw []byte, tiles map[string]string) []byte {
	var out bytes.Buffer
	for _, line := range strings.Split(string(raw), "\n") {
		if src, dst := origEnds(line); tiles[src] != "" || tiles[dst] != "" {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

// origEnds is the first src= and dst= of a conntrack line (the original
// direction).
func origEnds(line string) (src, dst string) {
	for _, f := range strings.Fields(line) {
		switch {
		case src == "" && strings.HasPrefix(f, "src="):
			src = f[4:]
		case dst == "" && strings.HasPrefix(f, "dst="):
			dst = f[4:]
		}
	}
	return src, dst
}

// enableAcct turns on conntrack byte counters (join writes the sysctl.d
// file too); a node without them has no traffic, so say so.
func enableAcct() {
	if b, err := os.ReadFile(acctKnob); err == nil && strings.TrimSpace(string(b)) == "1" {
		return
	}
	if err := os.WriteFile(acctKnob, []byte("1\n"), 0o644); err != nil {
		slog.Warn("worker: conntrack byte counters are off; tile traffic stays empty", "err", err)
	}
}
