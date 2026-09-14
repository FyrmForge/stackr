// Package nodes keeps the servers table in step with the swarm, pings the
// nodes, and issues the one-time keys a node joins with.
//
// The swarm is the truth about which nodes exist and what state they are in;
// the table exists for the things swarm has no opinion on, the display name
// the operator typed, and a row that exists before its node does
// (docs/plans/32-multi-node-ui.md).
package nodes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/hostmetrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/metrics"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// LocalID is the row that predates the swarm work: the manager, and the node
// the panel runs on.
const LocalID = "local"

// KeyTTL is how long a join key lives. Long enough to walk to another
// machine and paste a command, short enough that a key left in a chat log is
// not a way into the swarm.
const KeyTTL = time.Hour

// swarmPort is the port a ping opens. Not ICMP and not a new listener: the
// node already has to accept 7946 for swarm to work at all, so a TCP connect
// to it measures the path that actually matters.
const swarmPort = 7946

// Service is the servers screen's back end.
type Service struct {
	Store repo.Store
	RT    *runtime.Runtime
}

// Row is one line of the servers list: the table row and the swarm node
// folded together.
type Row struct {
	Server repo.Server
	Node   *runtime.Node // nil while the row is pending
	// Status is the word the list shows: pending, ready, draining or down.
	Status     string
	Containers int
	// PingMs is the last measured round trip, -1 when it failed,
	// PingUnknown when nothing has measured it yet, and 0 for
	// the manager's own row, which reads "self".
	PingMs float64
	// JoinKey is the key still on offer for a pending row, nil once used or
	// expired.
	JoinKey *repo.JoinKey
}

// Sync reconciles `docker node ls` into the servers table and returns the
// list to render.
//
// Nodes that swarm knows about and the table does not get a row: that is a
// node joined by hand, or one whose pending row was matched by hostname.
// Rows whose node has left are kept, not deleted, the operator removed the
// node, and losing the name they gave it in the same click is worse than a
// stale row they can delete themselves.
func (s *Service) Sync(ctx context.Context) ([]Row, error) {
	servers, err := s.Store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := s.RT.ListNodes(ctx)
	if err != nil {
		// Not a swarm, or the daemon is down: show the table alone rather
		// than an error page. A single-node install that never joined a
		// swarm still has its local row.
		slog.Debug("nodes: listing swarm nodes", "error", err)
		nodes = nil
	}
	counts, _ := s.RT.TasksByNode(ctx)

	byID := map[string]runtime.Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}

	claimed := map[string]bool{}
	pending := 0
	var rows []Row
	for i := range servers {
		sv := servers[i]
		n := s.match(ctx, &sv, byID, claimed)
		row := Row{Server: sv, Node: n, Containers: counts[sv.NodeID]}
		if n == nil {
			// A row Add node created whose swarm node has not appeared yet.
			row.Status = "pending"
			pending++
			if k, err := s.Store.LatestJoinKey(ctx, sv.ID); err == nil && k != nil && !k.Spent() {
				row.JoinKey = k
			}
		} else {
			row.Status = n.Status()
		}
		rows = append(rows, row)
	}

	// A node nobody has a row for. Adopt it rather than hide it: an operator
	// who ran `docker swarm join` by hand still needs to see their node.
	//
	// Not while a row is still pending, though. A node advertises whichever
	// address its daemon was started with, which is often not the one the
	// operator typed on Add node, a box added by its LAN address and
	// advertising a tunnel address matches neither on address nor on
	// hostname, and adopting it here would put the same machine on the list
	// twice. Waiting means the pending row shows for one more load and is
	// then matched by the join itself, which knows both.
	for _, n := range nodes {
		if claimed[n.ID] || pending > 0 {
			continue
		}
		sv := &repo.Server{
			ID: uuid.New().String(), Name: n.Hostname, Kind: "swarm",
			NodeID: n.ID, Hostname: n.Hostname, Address: n.Addr,
			Role: n.Role, Status: n.Status(), CreatedAt: time.Now().UTC(),
			Settings: "{}",
		}
		if err := s.Store.CreateServer(ctx, sv); err != nil {
			slog.Error("nodes: adopting a node with no row", "node", n.ID, "error", err)
			continue
		}
		node := n
		rows = append(rows, Row{Server: *sv, Node: &node, Status: n.Status(), Containers: counts[n.ID]})
	}

	for i := range rows {
		rows[i].PingMs = s.lastPing(ctx, rows[i])
	}
	return rows, nil
}

// match pairs a table row with its swarm node, adopting a pending row the
// first time its node appears.
//
// A pending row has no node ID yet, so it is matched on the address the
// operator typed. That is the same address the join key is bound to, which
// is why the key is bound to one at all.
func (s *Service) match(ctx context.Context, sv *repo.Server, byID map[string]runtime.Node, claimed map[string]bool) *runtime.Node {
	if sv.NodeID != "" {
		// claimed is checked here as well as in the loop below. One node
		// belongs to one row: without this, a row holding a stale ID can
		// re-attach by address while a second row still holds the ID it was
		// mirrored, and both then report the same node as theirs. Two rows
		// for one node means GetServerByNodeID answers with whichever the
		// database reaches first, so host samples split between two server
		// IDs and Remove on either drains the real machine.
		if n, ok := byID[sv.NodeID]; ok && !claimed[n.ID] {
			claimed[n.ID] = true
			s.mirror(ctx, sv, n)
			return &n
		}
		// The id is stale, not absent: swarm issues a new node ID on every
		// join, so a node removed and rejoined leaves its row pointing at an
		// ID that no longer exists. Falling through to the address match
		// below is what lets the row re-attach and mirror the new ID onto
		// itself. Returning nil here instead left the row reading "pending"
		// for good, offering a join script that could never resolve, and,
		// because Sync skips adoption while anything is pending, stopped
		// any hand-joined node from being adopted either.
	}
	// The local row is the manager, whichever node that is.
	for _, n := range byID {
		if claimed[n.ID] {
			continue
		}
		// No hostname clause. A pending row has no hostname at all (AddNode
		// writes none, only mirror ever sets one), so it never matched
		// anything a pending row could use, and a hostname collision between
		// two machines would attach the wrong node to the wrong row.
		hit := (sv.ID == LocalID && n.Self) ||
			(sv.Address != "" && n.Addr == sv.Address)
		if !hit {
			continue
		}
		claimed[n.ID] = true
		s.mirror(ctx, sv, n)
		return &n
	}
	return nil
}

// mirror writes swarm's view onto the row. The display name is never touched,
// that half is the operator's.
func (s *Service) mirror(ctx context.Context, sv *repo.Server, n runtime.Node) {
	before := *sv
	sv.NodeID, sv.Hostname, sv.Role, sv.Status = n.ID, n.Hostname, n.Role, n.Status()
	if n.Addr != "" {
		sv.Address = n.Addr
	}
	if sv.Name == "" {
		sv.Name = n.Hostname
	}
	sv.LastSeenAt.Time, sv.LastSeenAt.Valid = time.Now().UTC(), true
	if before.NodeID == sv.NodeID && before.Status == sv.Status &&
		before.Hostname == sv.Hostname && before.Role == sv.Role && before.Address == sv.Address {
		// Only the timestamp moved; not worth a write every page load.
		return
	}
	if err := s.Store.UpdateServer(ctx, sv); err != nil {
		slog.Error("nodes: mirroring swarm state", "server", sv.ID, "error", err)
	}
}

// --- ping -----------------------------------------------------------------

// PingRef is the metrics ref one node's ping samples are stored under, so the
// server page can chart them with the same code as the host graphs.
//
// The milliseconds go in Metric.CPUPct: one row shape serves every series,
// and a ping has no memory or byte counters to fill the other columns. The
// server page has to read the same slot back, which it did not.
func PingRef(serverID string) string { return "server:" + serverID + ":ping" }

// Ping measures every node once and stores the result. Called on a ticker.
func (s *Service) Ping(ctx context.Context) {
	servers, err := s.Store.ListServers(ctx)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for i := range servers {
		sv := servers[i]
		if sv.Address == "" || sv.ID == LocalID {
			continue // the manager pings everyone else, not itself
		}
		ms := pingOnce(ctx, sv.Address)
		// A failure is stored as a negative, so a gap in the chart means the
		// panel was down and a red point means the node was.
		_ = s.Store.InsertMetric(ctx, &repo.Metric{Ref: PingRef(sv.ID), TS: now, CPUPct: ms})
	}
}

// PingUnknown is "nobody has measured this node yet", which is not the same
// answer as "the node did not reply" and must not be shown as one. A node that
// joined less than one ping tick ago read as a red "timeout", which is an
// alarming thing to say about a node that is perfectly fine.
const PingUnknown = -2

// pingOnce times a TCP connect to the node's swarm port. Returns -1 on
// failure, which is what the list shows as "timeout".
func pingOnce(ctx context.Context, addr string) float64 {
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(addr, strconv.Itoa(swarmPort)))
	if err != nil {
		return -1
	}
	_ = conn.Close()
	return float64(time.Since(start).Microseconds()) / 1000
}

// lastPing reads the most recent stored sample for a row.
func (s *Service) lastPing(ctx context.Context, row Row) float64 {
	if row.Node != nil && row.Node.Self {
		return 0
	}
	ms, err := s.Store.ListMetrics(ctx, PingRef(row.Server.ID), time.Now().Add(-5*time.Minute))
	if err != nil || len(ms) == 0 {
		// No sample, not a failed one. The ticker runs every 30s, so a node
		// added seconds ago has none yet, and the panel having been down for
		// five minutes is also this, not a node that stopped answering.
		return PingUnknown
	}
	return ms[len(ms)-1].CPUPct
}

// --- samples --------------------------------------------------------------

// StoreSample records a host metrics sample an agent posted for its node.
func (s *Service) StoreSample(ctx context.Context, nodeID string, ts time.Time, sample hostmetrics.HostSample) error {
	sv, err := s.Store.GetServerByNodeID(ctx, nodeID)
	if err != nil {
		return err
	}
	if sv == nil {
		return fmt.Errorf("no server row for node %s", nodeID)
	}
	metrics.StoreHostSample(ctx, s.Store, "server:"+sv.ID, ts, sample)
	return nil
}

// --- joining --------------------------------------------------------------

// ValidateAddress refuses anything that is not a private address.
//
// The overlay data plane is plain VXLAN. A node reachable over the public
// internet means every byte between tiles crosses it in the clear unless
// encryption is on, and even then the swarm ports are exposed to everyone,
// so the address has to be a LAN, a provider private network, or a tunnel
// (docs/plans/30-docker-swarm.md, step 8, addresses).
func ValidateAddress(addr string) error {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return fmt.Errorf("%q is not an IP address; a node is added by address, not by name", addr)
	}
	// netip's IsPrivate covers RFC1918 but not the carrier-grade range
	// 100.64/10, which is where Tailscale and most provider tunnels live.
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	if !ip.IsPrivate() && !cgnat.Contains(ip) && !ip.IsLoopback() {
		return fmt.Errorf("%s is a public address; nodes join over a private network or a tunnel, "+
			"because the overlay between them is plain VXLAN", addr)
	}
	return nil
}

// AddNode creates the pending row and its one-time join key.
//
// One row per address. Adding the same machine twice used to be allowed, and
// the second row is not harmless: both rows then compete for the same swarm
// node, only one can claim it, and the loser reads "pending" for good while
// Sync's adoption loop stays disabled behind it. The operator is told which
// row already holds the address rather than being given a duplicate.
func (s *Service) AddNode(ctx context.Context, name, addr string) (*repo.Server, *repo.JoinKey, error) {
	if err := ValidateAddress(addr); err != nil {
		return nil, nil, err
	}
	// The join script is a root shell script and carries the name in a
	// comment line (handler/server/joinscript.go). A line break there adds
	// lines to the script. Refused at the writer every caller routes through
	// rather than quoted at the one render: the name reaches flash messages,
	// audit lines and the node page too, and none of those want it either.
	if strings.ContainsAny(name, "\r\n") {
		return nil, nil, fmt.Errorf("a server name cannot contain a line break")
	}
	existing, err := s.Store.ListServers(ctx)
	if err != nil {
		return nil, nil, err
	}
	for i := range existing {
		if existing[i].Address != addr {
			continue
		}
		return nil, nil, fmt.Errorf("%s is already added as %q; use its row to get a new join script, "+
			"or remove it first", addr, existing[i].Name)
	}
	if name == "" {
		name = addr
	}
	now := time.Now().UTC()
	sv := &repo.Server{
		ID: uuid.New().String(), Name: name, Kind: "swarm",
		Address: addr, Role: "worker", Status: "pending",
		Settings: "{}", CreatedAt: now,
	}
	if err := s.Store.CreateServer(ctx, sv); err != nil {
		return nil, nil, err
	}
	key, err := s.IssueKey(ctx, sv)
	if err != nil {
		return nil, nil, err
	}
	return sv, key, nil
}

// IssueKey mints a fresh join key for a pending row, which is what the "new
// script" button does.
//
// It spends the row's existing keys first. Issuing without revoking is what
// made "new script" look like a rotation and act like an addition: the key the
// operator meant to replace kept working for the rest of its hour, and nothing
// in the product could stop it. One node has one
// live key by construction, so a leaked script dies the moment a new one is
// generated.
func (s *Service) IssueKey(ctx context.Context, sv *repo.Server) (*repo.JoinKey, error) {
	if _, err := s.Store.BurnServerJoinKeys(ctx, sv.ID); err != nil {
		return nil, err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	k := &repo.JoinKey{
		Key: hex.EncodeToString(b), ServerID: sv.ID, Address: sv.Address,
		ExpiresAt: now.Add(KeyTTL), CreatedAt: now,
	}
	if err := s.Store.CreateJoinKey(ctx, k); err != nil {
		return nil, err
	}
	return k, nil
}

// Claim attaches a swarm node id to the row its join key was issued for.
//
// The address match on its own could not be relied on: n.Addr is the daemon's
// swarm advertise address and sv.Address is what the operator typed, and those
// routinely differ (a machine with several interfaces, a host advertising a
// tailscale address, a LAN address typed for a node that advertises its
// bridge). When they did, the row read "pending" for ever, offered a join
// script that resolved nothing, and, because Sync skips adoption while
// anything is pending, stopped every other node from being adopted too. The
// joining machine knows its own id, so it says so.
//
// The key is re-read rather than a second nonce minted: the row exists, the
// key already carries a TTL and the private-source rail, and the callback
// happens seconds after the join. It is accepted spent but not expired, which
// is what JoinKey's separate UsedAt and ExpiresAt are for.
//
// Three refusals stand between this and a hijack: inside the TTL, from a
// private address, and an id that is really a node in this swarm and is not
// already held by another row. The key's own row may be overwritten: a node
// removed and re-added carries a stale id, and the key was minted for exactly
// that row.
func (s *Service) Claim(ctx context.Context, key, nodeID, remoteAddr string) error {
	if nodeID == "" {
		return fmt.Errorf("no node id reported")
	}
	k, err := s.Store.GetJoinKey(ctx, key)
	if err != nil {
		return err
	}
	if k == nil {
		return fmt.Errorf("unknown join key")
	}
	if time.Now().After(k.ExpiresAt) {
		return fmt.Errorf("this join key has expired")
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		remoteAddr = host
	}
	// Same rail as Redeem, and loose for the same reason: the address the
	// panel observes is routinely the gateway's. What it catches is a key used
	// from outside the private network entirely.
	if remoteAddr != "" && remoteAddr != k.Address {
		if err := ValidateAddress(remoteAddr); err != nil {
			return fmt.Errorf("this key was issued for %s and the request came from %s, "+
				"which is outside your private network", k.Address, remoteAddr)
		}
	}
	nodes, err := s.RT.ListNodes(ctx)
	if err != nil {
		return err
	}
	known := false
	for _, n := range nodes {
		if n.ID == nodeID {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("%s is not a node in this swarm", nodeID)
	}
	servers, err := s.Store.ListServers(ctx)
	if err != nil {
		return err
	}
	for i := range servers {
		if servers[i].NodeID == nodeID && servers[i].ID != k.ServerID {
			return fmt.Errorf("%s is already held by %q", nodeID, servers[i].Name)
		}
	}
	sv, err := s.Store.GetServer(ctx, k.ServerID)
	if err != nil {
		return err
	}
	if sv == nil {
		return fmt.Errorf("the row this key was issued for is gone")
	}
	sv.NodeID = nodeID
	return s.Store.UpdateServer(ctx, sv)
}

// Redeem checks a join key and burns it, returning the row it belongs to.
//
// One-time and one hour are what actually protect the swarm. The address the
// key was issued for is a second rail on top, and a deliberately loose one:
// the joining machine reaches the panel through traefik and, on any LAN whose
// panel has a public DNS name, through the gateway's NAT as well, so the
// address the panel observes is routinely the router's and not the node's.
// Refusing on that mismatch would refuse every ordinary join.
//
// What the rail still catches is the case worth catching: a key used from
// outside the private network entirely. A public source address means the key
// left the building, and no legitimate node joins from one, because the whole
// swarm is private by rule (ValidateAddress).
func (s *Service) Redeem(ctx context.Context, key, remoteAddr string) (*repo.Server, error) {
	k, err := s.Store.GetJoinKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, fmt.Errorf("unknown join key")
	}
	if k.Spent() {
		return nil, fmt.Errorf("this join key has already been used or has expired; issue a new one from the servers page")
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		remoteAddr = host
	}
	if remoteAddr != "" && remoteAddr != k.Address {
		if err := ValidateAddress(remoteAddr); err != nil {
			return nil, fmt.Errorf("this key was issued for %s and the request came from %s, "+
				"which is outside your private network; issue a new key and run it on the node itself", k.Address, remoteAddr)
		}
		slog.Info("join key redeemed from a different private address than it was issued for",
			"issued_for", k.Address, "seen_from", remoteAddr,
			"note", "normal behind NAT or a reverse proxy")
	}
	ok, err := s.Store.BurnJoinKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("this join key has already been used")
	}
	return s.Store.GetServer(ctx, k.ServerID)
}
