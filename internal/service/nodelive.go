package service

import (
	"context"
	"fmt"
	"math"
	"net/netip"
	"runtime"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// controllerFacts is what the controller node reports about itself. On a
// multi-node install the mesh already reads it through localMesh, the same
// code a worker runs; a single node or a fake host has no mesh, so the
// facts are built from the interfaces and the daemon it talks to.
func (o *Orchestrator) controllerFacts(ctx context.Context, n store.Node) *worker.Facts {
	if o.mesh != nil {
		if f, ok := o.mesh.factsOf(n.ID); ok {
			return &f
		}
	}
	f := worker.Facts{Interfaces: o.hostIfaces(), Arch: runtime.GOARCH, Version: o.cfg.Version}
	if h, err := o.docker.HostInfo(docker.WithNode(ctx, n.ID)); err == nil {
		f.CPUs, f.MemBytes, f.Docker = h.CPUs, uint64(h.MemBytes), h.Version
	}
	f.DiskFree, f.DiskTotal = o.dataDirDisk()
	return &f
}

// NodeLive is what the node drawer shows live: the node's facts, when the
// controller last heard from it and its addresses grouped by kind.
type NodeLive struct {
	Facts    *worker.Facts // controller via controllerFacts, worker via reportedFacts
	LastSeen time.Time     // when the facts last came in; the controller: now
	Ifaces   []NodeIface
	// Behind: a worker not on the panel's build. TooOld: its build is not one
	// the panel still talks to. CertExpired: no live certificate, rejoin it.
	// CertDays: the live certificate ends in that many days, under 14 (0 = not).
	Behind, TooOld, CertExpired bool
	CertDays                    int
	Warnings                    []NodeWarning
}

// certWarnDays is how close to its end a certificate gets a note.
const certWarnDays = 14

// certNote is the certificate rule of the node drawer and the map card: of
// the certificates not revoked, the newest decides. Past its end (with
// ClockSlack, the rule of certExpired): expired. Under certWarnDays to go:
// the whole days left, a part of a day counting as one. Nothing live (all
// revoked, or none) says nothing: that node is mid-rejoin, not expired.
func certNote(certs []store.NodeCert, now time.Time) (expired bool, days int) {
	newest := newestCert(certs)
	switch {
	case newest.IsZero():
		return false, 0
	case now.Add(-clusterca.ClockSlack).After(newest):
		return true, 0
	}
	if d := max(1, int(math.Ceil(newest.Sub(now).Hours()/24))); d < certWarnDays {
		return false, d
	}
	return false, 0
}

// NodeWarning is something off on a node that stops nothing: the map card's
// badge (Label), the drawer's and `node show`'s line (Detail), `node ls`'s
// WARN column. Offline is not one: the status says it.
type NodeWarning struct {
	Kind     string `json:"kind"`     // clock | disk | cert | version | memory
	Severity string `json:"severity"` // warning | critical
	Label    string `json:"label"`
	Detail   string `json:"detail"`
}

// Warning kinds.
const (
	WarnClock   = "clock"
	WarnDisk    = "disk"
	WarnCert    = "cert"
	WarnVersion = "version"
	WarnMemory  = "memory"
)

// warnIn is what warningsOf judges; nodeWarnings gathers it.
type warnIn struct {
	Version             string // the build the node reported
	Panel               string // the panel's build
	Behind, TooOld      bool
	CertExpired         bool
	CertDays            int
	Skew                time.Duration
	SkewOK              bool
	DiskFree, DiskTotal uint64
	MemFree, MemTotal   uint64
}

// warningsOf is the one rule for a node's warnings, on the same lines as the
// alerts: skewed, diskLow, certNote, workerBehind.
func warningsOf(w warnIn) []NodeWarning {
	out := []NodeWarning{}
	ver := w.Version
	if ver == "" {
		ver = "an older build"
	}
	switch {
	case w.TooOld:
		out = append(out, NodeWarning{WarnVersion, SevCritical, "too old",
			"Runs " + ver + ", which the panel no longer drives. Update it in Server settings, Update."})
	case w.Behind:
		out = append(out, NodeWarning{WarnVersion, SevWarning, "behind",
			"Runs " + ver + ", the panel runs " + w.Panel + "."})
	}
	switch {
	case w.CertExpired:
		out = append(out, NodeWarning{WarnCert, SevCritical, "Cert expired, rejoin needed",
			"Its certificate expired. Rejoin it."})
	case w.CertDays > 0:
		d := fmt.Sprintf("%d days", w.CertDays)
		if w.CertDays == 1 {
			d = "1 day"
		}
		out = append(out, NodeWarning{WarnCert, SevWarning, "Cert expires in " + d,
			"Its certificate expires in " + d + "."})
	}
	if w.SkewOK && skewed(w.Skew) {
		out = append(out, NodeWarning{WarnClock, SevWarning, "Clock " + skewWords(w.Skew),
			"Its clock is " + skewWords(w.Skew) + " of the panel. Check time sync on the node."})
	}
	if (w.DiskFree != 0 || w.DiskTotal != 0) && diskLow(w.DiskFree, w.DiskTotal) {
		pct := uint64(0)
		if w.DiskTotal > 0 {
			pct = w.DiskFree * 100 / w.DiskTotal
		}
		out = append(out, NodeWarning{WarnDisk, SevWarning, "Disk low",
			fmt.Sprintf("%s (%d%%) free on its Docker disk.", gb(w.DiskFree), pct)})
	}
	if memLow(w.MemFree, w.MemTotal) {
		out = append(out, NodeWarning{WarnMemory, SevWarning, "Memory low",
			fmt.Sprintf("%s (%d%%) of memory available.", gb(w.MemFree), w.MemFree*100/w.MemTotal)})
	}
	return out
}

// nodeWarnings is a node's warnings; f is its facts (nil = none known).
func (o *Orchestrator) nodeWarnings(ctx context.Context, n store.Node, f *worker.Facts) []NodeWarning {
	w := warnIn{Panel: o.cfg.Version}
	if f != nil {
		w.Version, w.DiskFree, w.DiskTotal = f.Version, f.DiskFree, f.DiskTotal
		w.MemFree, w.MemTotal = f.MemFree, f.MemBytes
		w.Behind = o.workerBehind(n, f)
		w.TooOld = w.Behind && !o.versionAllowed(ctx, f.Version)
	}
	if n.Role == node.Worker {
		if cs, err := o.store.NodeCerts.ListByNode(ctx, n.ID); err == nil {
			w.CertExpired, w.CertDays = certNote(cs, time.Now())
		}
		w.Skew, w.SkewOK = o.skewOf(n.ID)
	}
	return warningsOf(w)
}

// newestCert is the latest end of a cert not revoked; zero = none.
func newestCert(certs []store.NodeCert) time.Time {
	var newest time.Time
	for _, c := range certs {
		if c.RevokedAt == nil && c.NotAfter.After(newest) {
			newest = c.NotAfter
		}
	}
	return newest
}

// NodeIface is one address of a node. Group is public, lan, cluster, docker
// or hidden (loopback, link-local, veth: kept out of the way).
type NodeIface struct{ Group, Name, Addr, Label string }

const (
	IfacePublic  = "public"
	IfaceLAN     = "lan"
	IfaceCluster = "cluster"
	IfaceDocker  = "docker"
	IfaceHidden  = "hidden"
)

// NodeLive reads a node's facts, last-seen time and grouped interfaces.
func (o *Orchestrator) NodeLive(ctx context.Context, id string) (NodeLive, error) {
	n, err := o.store.Nodes.Get(ctx, id)
	if err != nil {
		return NodeLive{}, err
	}
	out := NodeLive{Facts: o.reportedFacts(ctx, n)}
	out.Warnings = o.nodeWarnings(ctx, n, out.Facts)
	if n.Role == node.Controller {
		out.LastSeen = time.Now()
	} else {
		out.LastSeen = o.lastSeen(n.ID)
		cs, err := o.store.NodeCerts.ListByNode(ctx, n.ID)
		if err != nil {
			return out, err
		}
		out.CertExpired, out.CertDays = certNote(cs, time.Now())
		if f := out.Facts; f != nil {
			out.Behind = o.workerBehind(n, f)
			out.TooOld = !o.versionAllowed(ctx, f.Version)
		}
	}
	if out.Facts == nil {
		return out, nil
	}
	envs := map[string]string{}
	es, err := o.store.Environments.ListByNode(ctx, n.ID)
	if err != nil {
		return out, err
	}
	for _, e := range es {
		if p, err := netip.ParsePrefix(e.Subnet); err == nil {
			if ne, err := o.nodeEnvOut(ctx, e); err == nil {
				envs[p.Masked().String()] = ne.Path
			}
		}
	}
	out.Ifaces = groupIfaces(*out.Facts, envs)
	return out, nil
}

// seenAt is when a node's facts last came in (zero: never).
func (m *meshSvc) seenAt(id string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.at[id]
}

// groupIfaces sorts every address of a node into a group, per address: a
// public NIC's fe80 is link-local, not public. envs maps an env subnet to
// its org/stack/env path.
// ponytail: bridges named by subnet, not by Docker network; a job's network shows as "stackr network".
func groupIfaces(f worker.Facts, envs map[string]string) []NodeIface {
	var out []NodeIface
	for _, i := range f.Interfaces {
		for _, s := range i.Prefixes {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				continue
			}
			a := p.Addr().Unmap()
			g, label := IfaceLAN, ""
			switch {
			case i.Name == "lo" || a.IsLoopback():
				g, label = IfaceHidden, "loopback"
			case a.IsLinkLocalUnicast():
				g, label = IfaceHidden, "link-local"
			case strings.HasPrefix(i.Name, "veth"):
				g, label = IfaceHidden, "veth"
			case i.Name == mesh.Iface:
				g, label = IfaceCluster, "tunnel"
			case strings.HasPrefix(i.Name, "br-"):
				if path, ok := envs[p.Masked().String()]; ok {
					g, label = IfaceCluster, path
				} else if mesh.Supernet.Contains(a) {
					g, label = IfaceCluster, "stackr network"
				} else {
					g = IfaceDocker
				}
			case i.Name == "docker0":
				g = IfaceDocker
			case a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a):
				g = IfacePublic
			}
			out = append(out, NodeIface{Group: g, Name: i.Name, Addr: s, Label: label})
		}
	}
	return out
}
