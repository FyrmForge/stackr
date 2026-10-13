package service

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/netip"
	"os"
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

var (
	joinNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	joinTags   = []string{node.TagIngress, node.TagEgress, node.TagBuilder}
)

// JoinTokenSpec is what an admin asks for.
type JoinTokenSpec struct {
	NodeName   string   `json:"node_name"`
	Tags       []string `json:"tags"`
	TTLMinutes int      `json:"ttl_minutes"` // 0 = one hour
}

// JoinTokenOut is a minted token. The token and the command carry it; both
// are shown once.
type JoinTokenOut struct {
	Token       string    `json:"token"`
	NodeName    string    `json:"node_name"`
	Tags        []string  `json:"tags"`
	ExpiresAt   time.Time `json:"expires_at"`
	Fingerprint string    `json:"ca_fingerprint"` // SHA-256 of the cluster CA cert, hex
	Command     string    `json:"command"`        // the one line to run on the new machine
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
	Status    string `json:"status"`
	NodeID    string `json:"node_id,omitempty"`
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
	name := strings.TrimSpace(spec.NodeName)
	if !joinNameRE.MatchString(name) || name == "controller" {
		return JoinTokenOut{}, errs.Invalidf("node_name", "use lowercase letters, digits and dashes, up to 32 characters")
	}
	for _, t := range spec.Tags {
		if !slices.Contains(joinTags, t) {
			return JoinTokenOut{}, errs.Invalidf("tags", "%q is not a tag; use %s", t, strings.Join(joinTags, ", "))
		}
	}
	if _, err := o.store.Nodes.GetBySlug(ctx, name); err == nil {
		return JoinTokenOut{}, errs.Conflictf("A node named %q already exists.", name)
	}
	return o.mintJoin(ctx, by, name, spec.Tags, spec.TTLMinutes)
}

// mintJoin is the token itself. A rejoin names a node that already exists
// (RejoinNode set it to needs-rejoin); its request then reuses the row.
func (o *Orchestrator) mintJoin(ctx context.Context, by *Principal, name string, tagList []string, ttlMinutes int) (JoinTokenOut, error) {
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
		token, row, err := o.joinTokens(tx).Mint(ctx, name, tags, by.User.ID, ttl)
		if err != nil {
			return err
		}
		fp := caFingerprint(ca)
		out = JoinTokenOut{
			Token: token, NodeName: name, Tags: tags, ExpiresAt: row.ExpiresAt, Fingerprint: fp,
			Command: "stackr-install join --panel " + o.panelURL() + " --token " + token + " --ca-sha256 " + fp,
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: jointoken.ActionToken, ActorID: by.User.ID, KeyID: by.KeyID,
			TargetKind: "join_token", TargetID: row.ID,
			Detail: "node " + name + " tags " + strings.Join(tags, ","),
		})
	})
	return out, err
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
		n, err := node.New(tx.Nodes).CreatePendingOrRejoin(ctx, row.NodeName, tags, req.WGPublicKey, StateNeedsRejoin)
		if err != nil {
			return err
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
	n, err := o.store.Nodes.Get(ctx, row.NodeID)
	if err != nil {
		return JoinStatus{}, err
	}
	ca, err := o.loadCA()
	if err != nil {
		return JoinStatus{}, err
	}
	st := JoinStatus{
		Status: JoinConfirmed, NodeID: n.ID, WGAddress: n.WGAddress,
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
func (o *Orchestrator) PendingJoins(ctx context.Context) ([]PendingJoin, error) {
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
	}
	return JoinedNode{ID: out.ID, Name: out.Name, WGAddress: out.WGAddress, Status: out.Status}, err
}

// RejectJoin (server admin) turns a pending node away and deletes its row; a
// pending rejoin goes back to needs-rejoin, `node rejoin` mints a fresh command.
func (o *Orchestrator) RejectJoin(ctx context.Context, by *Principal, nodeID string) error {
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

// reportedPrefixes is every interface prefix a joining node reported. A
// rejoin's box still carries the cluster's own networks (stackr0, the Docker
// bridges stackr made, which the installer names in stackr_bridges): those
// are skipped there, everything else is checked like a new node's.
func reportedPrefixes(r store.JoinToken, rejoin bool) []netip.Prefix {
	var f struct {
		Interfaces []struct {
			Name     string   `json:"name"`
			Prefixes []string `json:"prefixes"`
		} `json:"interfaces"`
		StackrBridges []string `json:"stackr_bridges"`
	}
	if json.Unmarshal([]byte(r.Facts), &f) != nil {
		return nil
	}
	var out []netip.Prefix
	for _, i := range f.Interfaces {
		if rejoin && (i.Name == mesh.Iface || i.Name == mesh.LegacyIface || slices.Contains(f.StackrBridges, i.Name)) {
			continue
		}
		for _, s := range i.Prefixes {
			if p, err := netip.ParsePrefix(s); err == nil {
				out = append(out, p)
			}
		}
	}
	return out
}
