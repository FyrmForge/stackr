package service

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/jointoken"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The join flow: an admin mints a token (join.token), the new machine posts
// it with a CSR (join.request) and waits as a pending node, an admin confirms
// (node.confirm, cert.issue) or rejects (node.reject), and the machine polls
// JoinStatus for its cert. Every step writes the security log in the same
// transaction as the act.

// Join token lifetime: default and ceiling.
const (
	JoinTokenTTL    = time.Hour
	JoinTokenMaxTTL = 24 * time.Hour
)

// joinStatusGrace is how long after its confirm a join token still answers
// /join/status (a var for tests).
var joinStatusGrace = time.Hour

var (
	joinNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	joinTags   = []string{node.TagIngress, node.TagEgress, node.TagBuilder, node.TagStorage}
)

// JoinTokenSpec is what an admin asks for.
type JoinTokenSpec struct {
	NodeName string `json:"node_name"`
	// NodeAddress is the node's own DNS name ("" = none), set on its row
	// when it calls in. Omitted: <name>.<panel address>, as the panel
	// fills it in, or none when that is taken or no panel address is set.
	NodeAddress *string  `json:"node_address,omitempty"`
	Tags        []string `json:"tags"`
	TTLMinutes  int      `json:"ttl_minutes"` // 0 = one hour
}

// JoinTokenOut is a minted token. The token and the command carry it; both
// are shown once.
type JoinTokenOut struct {
	Token       string    `json:"token"`
	NodeName    string    `json:"node_name"`
	NodeAddress string    `json:"node_address,omitempty"`
	Tags        []string  `json:"tags"`
	ExpiresAt   time.Time `json:"expires_at"`
	Fingerprint string    `json:"ca_fingerprint"` // SHA-256 of the cluster CA cert, hex
	Command     string    `json:"command"`        // the installer's join line, which the one-line join runs
	// OneLine is what to run on the new machine: curl the panel's join
	// script into sh. "" on a panel without a publicly trusted certificate:
	// a script fetched without one could be swapped on the way.
	OneLine string `json:"one_line,omitempty"`
	// TwoStep is the same without the panel's script, for such a panel:
	// download the installer from GitHub, then Command. "" = not needed.
	TwoStep string `json:"two_step,omitempty"`
}

// JoinRequest is what the new machine posts.
type JoinRequest struct {
	Token       string          `json:"token"`
	CSR         string          `json:"csr"`           // PEM
	WGPublicKey string          `json:"wg_public_key"` // base64
	Facts       json.RawMessage `json:"facts"`         // the worker's /v1/facts document
}

// JoinMeta is what the HTTP layer knows about the caller.
type JoinMeta struct {
	IP, UserAgent string
	HTTPS         bool
}

// Join statuses.
const (
	JoinPending   = "pending"
	JoinConfirmed = "confirmed"
	JoinRejected  = "rejected"
)

// JoinStatus is what the new machine polls for. Cert and CA are set once
// confirmed.
type JoinStatus struct {
	Status string `json:"status"`
	NodeID string `json:"node_id,omitempty"`
	// NodeName is the name the admin gave, once confirmed: the installer
	// keeps it, so an uninstall can ask for it.
	NodeName  string `json:"node_name,omitempty"`
	Range     string `json:"range,omitempty"`
	WGAddress string `json:"wg_address,omitempty"`
	CertPEM   string `json:"cert_pem,omitempty"`
	CAPEM     string `json:"ca_pem,omitempty"`
	// Mesh is the node's first mesh spec, once the controller has planned
	// it; the installer waits for it and the worker brings its tunnel up
	// from it at start.
	Mesh *worker.MeshSpec `json:"mesh,omitempty"`
}

// JoinedNode is a node an admin confirmed.
type JoinedNode struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Range     string `json:"range"`
	WGAddress string `json:"wg_address"`
	Status    string `json:"status"`
}

// PendingJoin is a node waiting for an admin.
type PendingJoin struct {
	NodeID      string          `json:"node_id"`
	Name        string          `json:"name"`
	Tags        []string        `json:"tags"`
	WGPublicKey string          `json:"wg_public_key"`
	Facts       json.RawMessage `json:"facts"`
	RequestedAt time.Time       `json:"requested_at"`
}

func (o *Orchestrator) joinTokens(tx store.Tx) *jointoken.Leaf { return jointoken.New(tx.JoinTokens) }

func (o *Orchestrator) loadCA() (*clusterca.CA, error) {
	ca, err := clusterca.LoadCA(o.cfg.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errs.Conflictf("The cluster CA is not set up on this panel yet.")
	}
	return ca, err
}

// caFingerprint names the current root in join commands.
func caFingerprint(ca *clusterca.CA) string { return clusterca.Fingerprint(ca.Cert) }

func (o *Orchestrator) panelURL() string {
	switch {
	case o.cfg.BaseURL != "":
		return strings.TrimRight(o.cfg.BaseURL, "/")
	case o.cfg.PanelDomain != "":
		return "https://" + o.cfg.PanelDomain
	}
	return ""
}

// CreateJoinToken (server admin) mints a token for a node name and tags.
func (o *Orchestrator) CreateJoinToken(ctx context.Context, by *Principal, spec JoinTokenSpec) (JoinTokenOut, error) {
	if err := nodeAdmin(by); err != nil {
		return JoinTokenOut{}, err
	}
	name := strings.TrimSpace(spec.NodeName)
	if !joinNameRE.MatchString(name) || name == "controller" {
		return JoinTokenOut{}, errs.Invalidf("node_name", "Use lowercase letters, digits and dashes, up to 32 characters.")
	}
	for _, t := range spec.Tags {
		if !slices.Contains(joinTags, t) {
			return JoinTokenOut{}, errs.Invalidf("tags", "%q is not a tag; use %s", t, strings.Join(joinTags, ", "))
		}
	}
	if _, err := o.store.Nodes.GetBySlug(ctx, name); err == nil {
		return JoinTokenOut{}, errs.Conflictf("A node named %q already exists.", name)
	}
	var (
		addr string
		err  error
	)
	if spec.NodeAddress != nil {
		if addr, err = o.checkNodeAddress(ctx, "", *spec.NodeAddress); err != nil {
			return JoinTokenOut{}, err
		}
	} else if s := o.NodeAddressSuffix(ctx); s != "" {
		addr, _ = o.checkNodeAddress(ctx, "", name+s) // taken: no address, set it on the node later
	}
	// A default exit can't be picked before the node exists, so a new node
	// always has egress; an admin can drop it once it is up.
	return o.mintJoin(ctx, by, name, addr, append(slices.Clone(spec.Tags), node.TagEgress), spec.TTLMinutes)
}

// mintJoin is the token itself. A rejoin names a node that already exists
// (RejoinNode set it to needs-rejoin); its request then reuses the row.
func (o *Orchestrator) mintJoin(ctx context.Context, by *Principal, name, address string, tagList []string, ttlMinutes int) (JoinTokenOut, error) {
	ttl := time.Duration(ttlMinutes) * time.Minute
	switch {
	case ttlMinutes < 0 || ttl > JoinTokenMaxTTL:
		return JoinTokenOut{}, errs.Invalidf("ttl_minutes", "at most %d", int(JoinTokenMaxTTL.Minutes()))
	case ttl == 0:
		ttl = JoinTokenTTL
	}
	ca, err := o.loadCA()
	if err != nil {
		return JoinTokenOut{}, err
	}
	tags := slices.Compact(slices.Sorted(slices.Values(tagList)))
	var out JoinTokenOut
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		token, row, err := o.joinTokens(tx).Mint(ctx, name, address, tags, by.User.ID, ttl)
		if err != nil {
			return err
		}
		fp := caFingerprint(ca)
		out = JoinTokenOut{
			Token: token, NodeName: name, NodeAddress: address, Tags: tags, ExpiresAt: row.ExpiresAt, Fingerprint: fp,
			Command: "stackr-install " + shLine(o.joinArgs(token, fp)),
		}
		if o.publicCert() {
			out.OneLine = "curl -fsSL " + shWord(o.panelURL()+"/join/"+token) + " | sh"
		} else {
			out.TwoStep = "curl -fLo stackr-install " + shWord(o.installerURL()) + " && chmod +x stackr-install\n./" + out.Command
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: jointoken.ActionToken, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "join_token", TargetID: row.ID,
			Detail: "node " + name + " tags " + strings.Join(tags, ","),
		})
	})
	return out, err
}

// joinArgs are the installer's join arguments. --version pins the worker to
// the panel's build, so an installer older than the panel still installs the
// panel's version; a plain dev build has no release to name.
func (o *Orchestrator) joinArgs(token, fp string) []string {
	args := []string{"join", "--panel", o.panelURL(), "--token", token, "--ca-sha256", fp}
	if v := o.releaseVersion(); v != "" {
		args = append(args, "--version", v)
	}
	return args
}

// releaseVersion is the panel's release without the v; "" for a dev build.
func (o *Orchestrator) releaseVersion() string {
	v := strings.TrimPrefix(o.cfg.Version, "v")
	if v == "dev" {
		return ""
	}
	return v
}

// publicCert is whether the panel's own URL is https on a certificate a
// fresh box trusts: Caddy gets the panel one from ACME unless TLS is off.
func (o *Orchestrator) publicCert() bool {
	return !o.cfg.TLSOff && strings.HasPrefix(o.panelURL(), "https://")
}

// installerURL is the GitHub download of the installer of the panel's
// release, the latest one for a dev build.
func (o *Orchestrator) installerURL() string {
	if v := o.releaseVersion(); v != "" {
		return "https://github.com/FyrmForge/stackr/releases/download/v" + v + "/stackr-install-linux-amd64"
	}
	return "https://github.com/FyrmForge/stackr/releases/latest/download/stackr-install-linux-amd64"
}

// RequestJoin spends the token and parks a pending node. Unauthenticated: the
// token is the credential. A bad request leaves the token unspent.
func (o *Orchestrator) RequestJoin(ctx context.Context, meta JoinMeta, req JoinRequest) (JoinStatus, error) {
	if !meta.HTTPS && !o.cfg.TLSOff {
		return JoinStatus{}, errs.Refusedf("Joining is only accepted over https.")
	}
	if err := checkCSR(req.CSR); err != nil {
		return JoinStatus{}, err
	}
	if b, err := base64.StdEncoding.DecodeString(req.WGPublicKey); err != nil || len(b) != 32 {
		return JoinStatus{}, errs.Invalidf("wg_public_key", "must be a base64 WireGuard public key")
	}
	facts := "{}"
	if len(req.Facts) > 0 {
		var m map[string]json.RawMessage
		if json.Unmarshal(req.Facts, &m) != nil {
			return JoinStatus{}, errs.Invalidf("facts", "must be a JSON object")
		}
		facts = string(req.Facts)
	}
	var st JoinStatus
	err := o.store.Tx(ctx, func(tx store.Tx) error {
		jt := o.joinTokens(tx)
		row, err := jt.Spend(ctx, req.Token)
		if err != nil {
			return err
		}
		var tags []string
		_ = json.Unmarshal([]byte(row.Tags), &tags)
		n, fresh, err := node.New(tx.Nodes).CreatePendingOrRejoin(ctx, row.NodeName, tags, req.WGPublicKey, StateNeedsRejoin)
		if err != nil {
			return err
		}
		if fresh && row.NodeAddress != "" { // a rejoin keeps the address it has
			n.Address = row.NodeAddress
			if err := tx.Nodes.Update(ctx, n); err != nil {
				return err
			}
		}
		if err := jt.Record(ctx, row, n.ID, req.CSR, req.WGPublicKey, facts); err != nil {
			return err
		}
		st = JoinStatus{Status: JoinPending, NodeID: n.ID}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: jointoken.ActionRequest, TargetKind: "node", TargetID: n.ID,
			IP: meta.IP, UserAgent: meta.UserAgent, Detail: "token " + row.ID + " node " + n.Name,
		})
	})
	return st, err
}

func checkCSR(s string) error {
	blk, _ := pem.Decode([]byte(s))
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		return errs.Invalidf("csr", "must be a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return errs.Invalidf("csr", "is not a valid certificate request")
	}
	return nil
}

// JoinStatus (token holder) is where a join request stands. Unknown, unspent
// tokens are one answer, ErrNotFound.
func (o *Orchestrator) JoinStatus(ctx context.Context, token string) (JoinStatus, error) {
	row, err := jointoken.New(o.store.JoinTokens).ByToken(ctx, token)
	if err != nil {
		return JoinStatus{}, err
	}
	switch {
	case row.RejectedAt != nil:
		return JoinStatus{Status: JoinRejected}, nil
	case row.CertPEM == "":
		return JoinStatus{Status: JoinPending, NodeID: row.NodeID}, nil
	}
	// The installer polls until it holds its mesh spec, minutes after the
	// confirm; after that the token has nothing left to open. The cert's
	// NotBefore is the confirm time (less the clock slack).
	if c, err := clusterca.ParseCertPEM([]byte(row.CertPEM)); err == nil && time.Since(c.NotBefore) > clusterca.ClockSlack+joinStatusGrace {
		return JoinStatus{}, errs.ErrNotFound
	}
	n, err := o.store.Nodes.Get(ctx, row.NodeID)
	if err != nil {
		return JoinStatus{}, err
	}
	ca, err := o.loadCA()
	if err != nil {
		return JoinStatus{}, err
	}
	st := JoinStatus{
		Status: JoinConfirmed, NodeID: n.ID, NodeName: n.Name, WGAddress: n.WGAddress,
		CertPEM: row.CertPEM, CAPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})),
	}
	if o.mesh != nil {
		if spec, ok := o.mesh.Spec(n.ID); ok {
			st.Mesh = &spec
		} else {
			o.mesh.Kick()
		}
	}
	return st, nil
}

// PendingJoins (server admin) lists nodes waiting to be confirmed.
func (o *Orchestrator) PendingJoins(ctx context.Context, by *Principal) ([]PendingJoin, error) {
	if err := nodeAdmin(by); err != nil {
		return nil, err
	}
	return o.pendingJoins(ctx)
}

func (o *Orchestrator) pendingJoins(ctx context.Context) ([]PendingJoin, error) {
	rows, err := jointoken.New(o.store.JoinTokens).Used(ctx)
	if err != nil {
		return nil, err
	}
	var out []PendingJoin
	for _, r := range rows {
		if r.RejectedAt != nil || r.CertPEM != "" || r.NodeID == "" {
			continue
		}
		var tags []string
		_ = json.Unmarshal([]byte(r.Tags), &tags)
		out = append(out, PendingJoin{
			NodeID: r.NodeID, Name: r.NodeName, Tags: tags, WGPublicKey: r.WGPublicKey,
			Facts: json.RawMessage(r.Facts), RequestedAt: *r.UsedAt,
		})
	}
	return out, nil
}

// ConfirmJoin (server admin) gives a pending node its transit address and issues its
// cert from the cluster CA. The worker fetches the cert through JoinStatus.
func (o *Orchestrator) ConfirmJoin(ctx context.Context, by *Principal, nodeID string) (JoinedNode, error) {
	if err := nodeAdmin(by); err != nil {
		return JoinedNode{}, err
	}
	row, err := jointoken.New(o.store.JoinTokens).ByNode(ctx, nodeID)
	if err != nil {
		return JoinedNode{}, err
	}
	if row.RejectedAt != nil || row.CertPEM != "" {
		return JoinedNode{}, errs.ErrNotFound
	}
	ca, err := o.loadCA()
	if err != nil {
		return JoinedNode{}, err
	}
	var out store.Node
	var rejoined bool
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		prev, _ := tx.Nodes.Get(ctx, nodeID) // a rejoin already has its address
		rejoined = prev.WGAddress != ""
		out, err = node.New(tx.Nodes).Confirm(ctx, nodeID, reportedPrefixes(row, rejoined))
		if err != nil {
			return err
		}
		if _, err := tx.NodeCerts.RevokeNode(ctx, out.ID, time.Now().UTC()); err != nil { // a rejoin retires the old certs
			return err
		}
		iss, err := ca.IssueNode([]byte(row.CSR), out.ID, time.Now())
		if err != nil {
			return err
		}
		if err := tx.NodeCerts.Create(ctx, store.NodeCert{
			Serial: iss.Serial, NodeID: out.ID, Role: iss.Role, Issuer: iss.Issuer, NotAfter: iss.NotAfter, CreatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		if err := o.joinTokens(tx).SetCert(ctx, row, string(iss.CertPEM)); err != nil {
			return err
		}
		a := audit(tx)
		if rejoined {
			if err := a.Append(ctx, securitylog.Event{
				Action: securitylog.NodeRejoined, ActorID: by.User.ID, KeyID: by.KeyID,
				TargetKind: "node", TargetID: out.ID, Detail: "node " + out.Name,
			}); err != nil {
				return err
			}
		}
		if err := a.Append(ctx, securitylog.Event{
			Action: jointoken.ActionConfirm, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "node", TargetID: out.ID, Detail: "transit " + out.WGAddress,
		}); err != nil {
			return err
		}
		return a.Append(ctx, securitylog.Event{
			Action: jointoken.ActionCert, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "node", TargetID: out.ID, Detail: "serial " + iss.Serial + " role " + iss.Role,
		})
	})
	if err == nil {
		o.kickMesh() // the controller adds the peer; the next refresh brings the worker online
		o.replanServerFile(ctx)
	}
	return JoinedNode{ID: out.ID, Name: out.Name, WGAddress: out.WGAddress, Status: out.Status}, err
}

// replanServerFile queues a re-plan of the bound server file, so a placement
// waiting for the node that just joined gets it without a push. Nothing is
// queued when no file is bound.
func (o *Orchestrator) replanServerFile(ctx context.Context) {
	if b, err := o.ServerConfigBinding(ctx); err != nil || b.Repo == "" {
		return
	}
	if _, err := o.enqueue(ctx, kindServerPlan, serverPlanJob{}, "serverconfig"); err != nil {
		slog.Warn("join: server file not re-planned", "err", err)
	}
}

// RejectJoin (server admin) turns a pending node away and deletes its row; a
// pending rejoin goes back to needs-rejoin, `node rejoin` mints a fresh command.
func (o *Orchestrator) RejectJoin(ctx context.Context, by *Principal, nodeID string) error {
	if err := nodeAdmin(by); err != nil {
		return err
	}
	row, err := jointoken.New(o.store.JoinTokens).ByNode(ctx, nodeID)
	if err != nil {
		return err
	}
	if row.RejectedAt != nil || row.CertPEM != "" {
		return errs.ErrNotFound
	}
	return o.store.Tx(ctx, func(tx store.Tx) error {
		n, err := node.New(tx.Nodes).Reject(ctx, nodeID, StateNeedsRejoin)
		if err != nil {
			return err
		}
		if err := o.joinTokens(tx).SetRejected(ctx, row, time.Now().UTC()); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: jointoken.ActionReject, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "node", TargetID: n.ID, Detail: "node " + n.Name,
		})
	})
}

// reportedPrefixes is every interface prefix a joining node reported, less
// stackr's own: stackr0 and the Docker bridges stackr made (the installer
// names them in stackr_bridges). A rejoin keeps them; a box that was a node
// before has its installer clear them once the join is confirmed. The
// legacy wg0 is skipped on a rejoin only: on a new box it may be anyone's.
func reportedPrefixes(r store.JoinToken, rejoin bool) []node.Iface {
	var f struct {
		Interfaces []struct {
			Name     string   `json:"name"`
			Prefixes []string `json:"prefixes"`
		} `json:"interfaces"`
		StackrBridges []string `json:"stackr_bridges"`
		// DockerNetworks names each bridge's Docker network, for the refusal.
		DockerNetworks map[string]string `json:"docker_networks"`
	}
	if json.Unmarshal([]byte(r.Facts), &f) != nil {
		return nil
	}
	var out []node.Iface
	for _, i := range f.Interfaces {
		if i.Name == mesh.Iface || slices.Contains(f.StackrBridges, i.Name) || rejoin && i.Name == mesh.LegacyIface {
			continue
		}
		name := i.Name
		if d, ok := f.DockerNetworks[i.Name]; ok {
			name = d + " (" + i.Name + ")"
		}
		for _, s := range i.Prefixes {
			if p, err := netip.ParsePrefix(s); err == nil {
				out = append(out, node.Iface{Name: name, Prefix: p})
			}
		}
	}
	return out
}

// installerPath is the stackr-install the panel hands to a joining box: the
// one next to stackrd (/stackr-install in the image, bin/ in development).
var installerPath = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "stackr-install")
}

// JoinScript (token holder, public) is the shell script `curl .../join/<token>
// | sh` runs on the new machine: check the box, fetch stackr-install from the
// panel into a temp dir, run its join with the same token. Only a live token
// gets one, and the token stays unspent. Every value in it is single quoted.
func (o *Orchestrator) JoinScript(ctx context.Context, meta JoinMeta, token string) (string, error) {
	if err := o.scriptServed(meta); err != nil {
		return "", err
	}
	if _, err := jointoken.New(o.store.JoinTokens).Live(ctx, token); err != nil {
		return "", err
	}
	panel := o.panelURL()
	ca, err := o.loadCA()
	if err != nil {
		return "", err
	}
	args := o.joinArgs(token, caFingerprint(ca))
	for i := range args {
		args[i] = shQuote(args[i])
	}
	return fmt.Sprintf(joinScript, shQuote(panel+"/join/"+token+"/stackr-install"), strings.Join(args, " ")), nil
}

// scriptServed refuses the join script and its download unless both travel
// over https on a certificate a fresh box trusts: either swapped on the way
// is root on the new machine.
func (o *Orchestrator) scriptServed(meta JoinMeta) error {
	if !o.publicCert() {
		return errs.Refusedf("This panel has no trusted certificate; use the two-step join command.")
	}
	if !meta.HTTPS {
		return errs.Refusedf("The join script is only served over https.")
	}
	return nil
}

// JoinInstaller (token holder, public) is the path of the stackr-install the
// join script downloads, for a live token only.
func (o *Orchestrator) JoinInstaller(ctx context.Context, meta JoinMeta, token string) (string, error) {
	if err := o.scriptServed(meta); err != nil {
		return "", err
	}
	if _, err := jointoken.New(o.store.JoinTokens).Live(ctx, token); err != nil {
		return "", err
	}
	p := installerPath()
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		slog.Warn("join: no stackr-install next to stackrd to hand out", "path", p)
		return "", errs.Conflictf("This panel has no stackr-install to hand out; download it from the GitHub release.")
	}
	return p, nil
}

// shQuote single quotes s for sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// shWord is s as one sh word: bare when it holds nothing sh reads, else
// quoted. The printed commands get pasted into a root shell.
func shWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:@%+=,") == "" {
		return s
	}
	return shQuote(s)
}

func shLine(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shWord(a)
	}
	return strings.Join(out, " ")
}

// joinScript takes the installer URL and the quoted join arguments. The body
// sits in a function so a cut-off download runs nothing; the installer reads
// /dev/null, never the rest of the piped script.
const joinScript = `#!/bin/sh
# stackr: join this machine to the cluster as a worker node.
set -eu

fail() { echo "stackr: $*" >&2; exit 1; }

main() {
	[ "$(uname -s)" = Linux ] || fail "a worker runs on Linux only"
	case "$(uname -m)" in
	x86_64 | amd64) ;;
	*) fail "a worker needs amd64, this machine is $(uname -m)" ;;
	esac
	[ "$(id -u)" = 0 ] || fail "run as root: pipe it to sudo sh"
	command -v docker >/dev/null 2>&1 || fail "install Docker 26.0 or newer first: https://docs.docker.com/engine/install/"
	docker info >/dev/null 2>&1 || fail "Docker is installed but not running"
	command -v curl >/dev/null 2>&1 || fail "curl is missing"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM
	curl -fsSL -o "$tmp/stackr-install" %s
	chmod 0755 "$tmp/stackr-install"
	"$tmp/stackr-install" %s </dev/null
}

main
`
