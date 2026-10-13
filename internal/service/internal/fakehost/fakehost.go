// Package fakehost is the laptop's Docker daemon and VIP table when
// STACKR_FAKE_HOST is set: a stateful stand-in, saved under one directory so
// it survives the restart hamr dev does on every save. Nothing here touches
// the host. Local development only; the rig and installs never set it.
//
// The test double is service/internal/dockerfake; this one keeps state so
// the panel sees tiles deploy, run and stop.
package fakehost

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// ErrExec is every exec on the fake host: managed databases and dump
// backups read real command output, so an echo would be a false success.
var ErrExec = errors.New("fakehost: exec is not available on the fake host; test it on the rig")

// roleRun marks a one-off run (leaf/tile's LabelRole "run"): it exits by itself.
const roleRun = "run"

type ctr struct {
	ID, Name, Image string
	State           string // created | running | paused | exited
	Started         string
	ExitCode        int
	Labels          map[string]string
	Env             []string
	Mounts          []string          // "source -> destination"
	Volumes         []string          // named volumes it mounts
	Ports           []string          // "ip:host -> container/proto"
	Nets            map[string]string // network -> IP
	HostNetwork     bool
	Healthcheck     bool
	HealthInterval  time.Duration
	HealthStart     time.Duration
	HealthRetries   int
}

func (c *ctr) running() bool { return c.State == "running" || c.State == "paused" }

type network struct {
	Name   string
	Subnet string // a /24 the caller reserved from a node range
	Labels map[string]string
}

type volume struct {
	Name, Driver, Created string
	Labels                map[string]string
}

type image struct {
	ID, Digest  string
	Tags        []string
	RepoDigests []string // repo@digest, set by a pull
	Labels      map[string]string
	Size        int64
}

type state struct {
	Containers map[string]*ctr     // by id
	Networks   map[string]*network // by name
	Volumes    map[string]*volume  // by name
	Images     map[string]*image   // by id
}

// Docker satisfies service.Docker.
type Docker struct {
	dir   string
	chaos *chaos

	mu    sync.Mutex
	st    state
	exits map[string]chan struct{} // container id -> closed when it stops
}

// New loads (or starts) the state under dir and seeds the proxy's stand-in.
func New(dir string) (*Docker, error) {
	if err := os.MkdirAll(filepath.Join(dir, "volumes"), 0o750); err != nil {
		return nil, err
	}
	d := &Docker{dir: dir, chaos: &chaos{path: filepath.Join(dir, "chaos.json")}, exits: map[string]chan struct{}{}}
	d.st = state{map[string]*ctr{}, map[string]*network{}, map[string]*volume{}, map[string]*image{}}
	b, err := os.ReadFile(d.statePath())
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &d.st); err != nil {
			return nil, fmt.Errorf("fakehost: %s: %w (delete it to start over)", d.statePath(), err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.st.Containers {
		// A run's exit timer died with the last process: it is over.
		if c.Labels["stackr.role"] == roleRun && c.running() {
			c.State, c.ExitCode = "exited", 137 // killed, never a pass
		}
		if c.running() {
			d.exits[c.ID] = make(chan struct{})
		}
	}
	// Domain code joins stackr-proxy to ingress networks and reads its
	// address; panel backup tars its volume. The real proxy runs outside
	// the fake (docker-compose.dev.yaml).
	if _, ok := d.st.Volumes[installspec.ProxyVolume]; !ok {
		d.st.Volumes[installspec.ProxyVolume] = &volume{Name: installspec.ProxyVolume, Driver: "local", Created: now()}
	}
	if d.find(installspec.ProxyName) == nil {
		id := newID()
		d.st.Containers[id] = &ctr{
			ID: id, Name: installspec.ProxyName, Image: "stackrd proxy (local)", State: "running", Started: now(),
			Labels:  map[string]string{docker.LabelManaged: "true", installspec.LabelRole: "proxy"},
			Mounts:  []string{installspec.ProxyVolume + " -> /caddy"},
			Volumes: []string{installspec.ProxyVolume},
			Nets:    map[string]string{},
		}
		d.exits[id] = make(chan struct{})
	}
	return d, d.save()
}

// Conntrack is an empty conntrack table under dir. It must exist: a missing
// path makes the traffic sample run conntrack -L.
func (d *Docker) Conntrack() (string, error) {
	p := filepath.Join(d.dir, "conntrack")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return "", err
	}
	return p, f.Close()
}

func (d *Docker) statePath() string { return filepath.Join(d.dir, "docker.json") }

func (d *Docker) archive(vol string) string {
	return filepath.Join(d.dir, "volumes", vol+".tgz")
}

// save writes the state; the caller holds mu.
func (d *Docker) save() error {
	b, err := json.MarshalIndent(d.st, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(d.statePath(), bytes.NewReader(b))
}

// writeFile is a temp file plus rename, so a kill never leaves half a file.
// Owner-only: the state holds container env, archives hold volume data.
func writeFile(path string, r io.Reader) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // a no-op after the rename
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// find resolves a container by id, name or id prefix, as the daemon does.
func (d *Docker) find(ref string) *ctr {
	if c, ok := d.st.Containers[ref]; ok {
		return c
	}
	for _, c := range d.st.Containers {
		if c.Name == ref || (len(ref) >= 12 && strings.HasPrefix(c.ID, ref)) {
			return c
		}
	}
	return nil
}

func notFound(kind, ref string) error {
	return fmt.Errorf("%w: no such %s: %s", docker.ErrNotFound, kind, ref)
}

func conflict(msg string) error { return fmt.Errorf("%w: %s", cerrdefs.ErrConflict, msg) }

func newID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// matches is the daemon's label filter: every wanted key=value present.
func matches(have, want map[string]string) bool {
	for k, v := range want {
		if hv, ok := have[k]; !ok || hv != v {
			return false
		}
	}
	return true
}

func withManaged(labels map[string]string) map[string]string {
	all := map[string]string{docker.LabelManaged: "true"}
	maps.Copy(all, labels)
	return all
}

// ---- Containers.

func (d *Docker) Run(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	id, err := d.Create(ctx, spec)
	if err != nil {
		return "", err
	}
	if err := d.Start(ctx, id); err != nil {
		_ = d.StopRemove(context.WithoutCancel(ctx), id)
		return "", err
	}
	return id, nil
}

func (d *Docker) Create(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	if err := d.chaos.hit(ctx, "containers", "Create"); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if spec.Name != "" && d.find(spec.Name) != nil {
		return "", conflict(fmt.Sprintf("the container name %q is already in use", spec.Name))
	}
	c := &ctr{
		ID: newID(), Name: spec.Name, Image: spec.Image, State: "created",
		Labels: withManaged(spec.Labels), Env: spec.Env, Nets: map[string]string{},
		HostNetwork: spec.HostNetwork, Healthcheck: spec.HealthCmd != "",
		HealthInterval: time.Duration(spec.HealthIntervalS) * time.Second,
		HealthStart:    time.Duration(spec.HealthStartPeriodS) * time.Second,
		HealthRetries:  spec.HealthRetries,
	}
	if c.Name == "" {
		c.Name = c.ID[:12]
	}
	if !spec.HostNetwork {
		for _, n := range spec.Networks {
			if _, ok := d.st.Networks[n.Name]; !ok {
				return "", notFound("network", n.Name) // before anything is created, as the daemon does
			}
		}
	}
	for _, v := range spec.Volumes {
		src, rest, _ := strings.Cut(v, ":")
		dst, _, _ := strings.Cut(rest, ":")
		c.Mounts = append(c.Mounts, src+" -> "+dst)
		if strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") {
			continue // a bind
		}
		c.Volumes = append(c.Volumes, src)
		if _, ok := d.st.Volumes[src]; !ok { // the daemon creates a missing named volume
			d.st.Volumes[src] = &volume{Name: src, Driver: "local", Created: now(), Labels: map[string]string{}}
		}
	}
	if !spec.HostNetwork {
		for host, cont := range spec.Ports {
			if !strings.Contains(cont, "/") {
				cont += "/tcp"
			}
			hp, _, _ := strings.Cut(host, "/")
			c.Ports = append(c.Ports, "0.0.0.0:"+hp+" -> "+cont)
		}
		for _, n := range spec.Networks {
			ip, err := d.assign(d.st.Networks[n.Name])
			if n.IPv4 != "" {
				ip, err = n.IPv4, nil
				for _, o := range d.st.Containers {
					if o.Nets[n.Name] == ip {
						err = fmt.Errorf("Address already in use: %s on %s", ip, n.Name)
					}
				}
			}
			if err != nil {
				return "", err
			}
			c.Nets[n.Name] = ip
		}
	}
	d.st.Containers[c.ID] = c
	return c.ID, d.save()
}

func (d *Docker) HostInfo(ctx context.Context) (docker.HostInfo, error) {
	if err := d.chaos.hit(ctx, "containers", "HostInfo"); err != nil {
		return docker.HostInfo{}, err
	}
	// ponytail: a fixed 8 cores / 16 GB so tile limit checks run; a chaos knob if a test needs a small host.
	return docker.HostInfo{CPUs: 8, MemBytes: 16 << 30, Arch: "x86_64"}, nil
}

func (d *Docker) Start(ctx context.Context, id string) error {
	if err := d.chaos.hit(ctx, "containers", "Start"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(id)
	if c == nil {
		return notFound("container", id)
	}
	if c.running() {
		return nil
	}
	d.start(c)
	return d.save()
}

// start marks c running and, for a one-off run, schedules its exit. The
// caller holds mu.
func (d *Docker) start(c *ctr) {
	c.State, c.Started, c.ExitCode = "running", now(), 0
	done := make(chan struct{})
	d.exits[c.ID] = done
	if c.Labels["stackr.role"] != roleRun {
		return
	}
	code := 0
	if d.chaos.runFails() {
		code = 1
	}
	wait := time.Duration(len(runLines)+1) * d.chaos.lineDelay()
	go func() {
		select {
		case <-done: // stopped first
		case <-time.After(wait):
			d.mu.Lock()
			if cur, ok := d.st.Containers[c.ID]; ok && cur.running() && d.exits[c.ID] == done {
				d.exit(cur, code)
				_ = d.save()
			}
			d.mu.Unlock()
		}
	}()
}

// exit marks c exited and wakes its waiters. The caller holds mu.
func (d *Docker) exit(c *ctr, code int) {
	c.State, c.ExitCode = "exited", code
	if ch, ok := d.exits[c.ID]; ok {
		close(ch)
		delete(d.exits, c.ID)
	}
}

func (d *Docker) Stop(ctx context.Context, id string) error {
	if err := d.chaos.hit(ctx, "containers", "Stop"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(id)
	if c == nil {
		return notFound("container", id)
	}
	if c.running() {
		d.exit(c, 0)
	}
	return d.save()
}

func (d *Docker) Restart(ctx context.Context, id string) error {
	if err := d.chaos.hit(ctx, "containers", "Restart"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(id)
	if c == nil {
		return notFound("container", id)
	}
	if c.running() {
		d.exit(c, 0)
	}
	d.start(c)
	return d.save()
}

func (d *Docker) StopRemove(ctx context.Context, id string) error {
	if err := d.chaos.hit(ctx, "containers", "StopRemove"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(id)
	if c == nil {
		return notFound("container", id)
	}
	if c.Name == installspec.ProxyName {
		return errors.New("fakehost: stackr-proxy stands in for the local proxy and stays")
	}
	if c.running() {
		d.exit(c, 0)
	}
	delete(d.st.Containers, c.ID)
	return d.save()
}

func (d *Docker) Rename(ctx context.Context, id, name string) error {
	if err := d.chaos.hit(ctx, "containers", "Rename"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(id)
	if c == nil {
		return notFound("container", id)
	}
	if o := d.find(name); o != nil && o != c {
		return conflict(fmt.Sprintf("the container name %q is already in use", name))
	}
	c.Name = name
	return d.save()
}

func (d *Docker) Pause(ctx context.Context, id string) error {
	return d.setPaused(ctx, "Pause", id, true)
}

func (d *Docker) Unpause(ctx context.Context, id string) error {
	return d.setPaused(ctx, "Unpause", id, false)
}

func (d *Docker) setPaused(ctx context.Context, method, id string, paused bool) error {
	if err := d.chaos.hit(ctx, "containers", method); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(id)
	if c == nil {
		return notFound("container", id)
	}
	switch {
	case paused && c.State == "running":
		c.State = "paused"
	case paused:
		return conflict("container " + c.Name + " is not running")
	case c.State == "paused":
		c.State = "running"
	}
	return d.save()
}

func (d *Docker) List(ctx context.Context, labels map[string]string) ([]docker.Container, error) {
	if err := d.chaos.hit(ctx, "containers", "List"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []docker.Container
	for _, c := range d.st.Containers {
		if !matches(c.Labels, labels) {
			continue
		}
		row := docker.Container{
			ID: c.ID, Name: c.Name, Image: c.Image, State: c.State, Health: c.health(),
			Labels: maps.Clone(c.Labels), HostNetwork: c.HostNetwork,
		}
		if c.running() {
			row.Nets = maps.Clone(c.Nets)
			for _, ip := range c.Nets {
				row.IPs = append(row.IPs, ip)
			}
		}
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b docker.Container) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// health is docker's word: a HEALTHCHECK on a running container passes.
func (c *ctr) health() string {
	if !c.Healthcheck || !c.running() {
		return ""
	}
	return "healthy"
}

func (d *Docker) Inspect(ctx context.Context, id string) (docker.Detail, error) {
	if err := d.chaos.hit(ctx, "containers", "Inspect"); err != nil {
		return docker.Detail{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(id)
	if c == nil {
		return docker.Detail{}, notFound("container", id)
	}
	det := docker.Detail{
		ID: c.ID, Name: c.Name, Image: c.Image, State: c.State, Started: c.Started,
		Running: c.running(), Health: c.health(),
		HealthInterval: c.HealthInterval, HealthStartPeriod: c.HealthStart, HealthRetries: c.HealthRetries,
		Ports: slices.Clone(c.Ports), Mounts: slices.Clone(c.Mounts), Env: slices.Clone(c.Env),
		Networks: map[string]string{}, HostNetwork: c.HostNetwork,
	}
	for net, ip := range c.Nets {
		if !c.running() {
			ip = "" // a stopped container keeps its networks, not its addresses
		}
		det.Networks[net] = ip
	}
	return det, nil
}

func (d *Docker) Wait(ctx context.Context, id string) (int, error) {
	if err := d.chaos.hit(ctx, "containers", "Wait"); err != nil {
		return -1, err
	}
	d.mu.Lock()
	c := d.find(id)
	if c == nil {
		d.mu.Unlock()
		return -1, notFound("container", id)
	}
	done, running := d.exits[c.ID], c.running()
	cid := c.ID
	d.mu.Unlock()
	if running && done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.st.Containers[cid]; ok {
		return c.ExitCode, nil
	}
	return 0, nil
}

// ---- Networks.

// assign hands out the lowest free host address of n (.1 is the gateway).
// The caller holds mu.
func (d *Docker) assign(n *network) (string, error) {
	used := map[string]bool{}
	for _, c := range d.st.Containers {
		used[c.Nets[n.Name]] = true
	}
	a := netip.MustParsePrefix(n.Subnet).Addr().As4()
	for h := 2; h < 255; h++ {
		a[3] = byte(h)
		if ip := netip.AddrFrom4(a).String(); !used[ip] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no available IPv4 addresses on network %s", n.Name)
}

func (n *network) gateway() string {
	a := netip.MustParsePrefix(n.Subnet).Addr().As4()
	a[3] = 1
	return netip.AddrFrom4(a).String()
}

// EnsureNetwork follows the real client: a zero subnet only asks whether the
// network is there (docker.ErrNeedsSubnet when not), a taken one is refused as
// an overlap. The subnet is the caller's; the fake never picks one.
func (d *Docker) EnsureNetwork(ctx context.Context, name string, subnet netip.Prefix, labels map[string]string) error {
	if err := d.chaos.hit(ctx, "networks", "EnsureNetwork"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.st.Networks[name]; ok {
		return nil
	}
	if !subnet.IsValid() {
		return docker.ErrNeedsSubnet
	}
	s := subnet.Masked().String()
	for _, n := range d.st.Networks {
		if n.Subnet == s {
			return fmt.Errorf("%w: Pool overlaps with other one on this address space", docker.ErrSubnetTaken)
		}
	}
	d.st.Networks[name] = &network{Name: name, Subnet: s, Labels: withManaged(labels)}
	return d.save()
}

// UsedSubnets are the subnets of the fake's networks; it has no host
// interfaces to add.
func (d *Docker) UsedSubnets(ctx context.Context) ([]netip.Prefix, error) {
	if err := d.chaos.hit(ctx, "networks", "UsedSubnets"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []netip.Prefix
	for _, n := range d.st.Networks {
		if p, err := netip.ParsePrefix(n.Subnet); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

func (d *Docker) RemoveNetwork(ctx context.Context, name string) error {
	if err := d.chaos.hit(ctx, "networks", "RemoveNetwork"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.st.Networks[name]; !ok {
		return nil
	}
	for _, c := range d.st.Containers {
		if _, on := c.Nets[name]; on && c.running() {
			return fmt.Errorf("error while removing network: network %s has active endpoints", name)
		}
	}
	for _, c := range d.st.Containers {
		delete(c.Nets, name) // a stopped container lets go, as the daemon does
	}
	delete(d.st.Networks, name)
	return d.save()
}

func (d *Docker) nets(labels map[string]string) []*network {
	var out []*network
	for _, n := range d.st.Networks {
		if matches(n.Labels, labels) {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b *network) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (d *Docker) ListNetworks(ctx context.Context, labels map[string]string) ([]string, error) {
	if err := d.chaos.hit(ctx, "networks", "ListNetworks"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, n := range d.nets(labels) {
		out = append(out, n.Name)
	}
	return out, nil
}

func (d *Docker) Gateways(ctx context.Context, labels map[string]string) ([]string, error) {
	if err := d.chaos.hit(ctx, "networks", "Gateways"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, n := range d.nets(labels) {
		out = append(out, n.gateway())
	}
	return out, nil
}

func (d *Docker) Connect(ctx context.Context, name, containerID string, _ []string) error {
	if err := d.chaos.hit(ctx, "networks", "Connect"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.st.Networks[name]
	if !ok {
		return notFound("network", name)
	}
	c := d.find(containerID)
	if c == nil {
		return notFound("container", containerID)
	}
	if _, on := c.Nets[name]; on {
		return nil
	}
	ip, err := d.assign(n)
	if err != nil {
		return err
	}
	c.Nets[name] = ip
	return d.save()
}

func (d *Docker) Disconnect(ctx context.Context, name, containerID string) error {
	if err := d.chaos.hit(ctx, "networks", "Disconnect"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.st.Networks[name]; !ok {
		return notFound("network", name)
	}
	c := d.find(containerID)
	if c == nil {
		return notFound("container", containerID)
	}
	delete(c.Nets, name)
	return d.save()
}

func (d *Docker) NetworkMembers(ctx context.Context, name string) ([]string, error) {
	if err := d.chaos.hit(ctx, "networks", "NetworkMembers"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, c := range d.st.Containers {
		if _, on := c.Nets[name]; on && c.running() {
			ids = append(ids, c.ID)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func (d *Docker) MemberAddr(ctx context.Context, name, containerID string) (string, string, error) {
	if err := d.chaos.hit(ctx, "networks", "MemberAddr"); err != nil {
		return "", "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.st.Networks[name]
	if !ok {
		return "", "", notFound("network", name)
	}
	if c := d.find(containerID); c != nil && c.running() {
		return c.Nets[name], n.Subnet, nil
	}
	return "", n.Subnet, nil
}

// ---- Volumes.

func (d *Docker) CreateVolume(ctx context.Context, name, driver string, _, labels map[string]string) error {
	if err := d.chaos.hit(ctx, "volumes", "CreateVolume"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.st.Volumes[name]; ok {
		return nil // the daemon's create is idempotent
	}
	if driver == "" {
		driver = "local"
	}
	d.st.Volumes[name] = &volume{Name: name, Driver: driver, Created: now(), Labels: withManaged(labels)}
	return d.save()
}

func (d *Docker) RemoveVolume(ctx context.Context, name string) error {
	if err := d.chaos.hit(ctx, "volumes", "RemoveVolume"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.st.Volumes[name]; !ok {
		return nil
	}
	for _, c := range d.st.Containers {
		if slices.Contains(c.Volumes, name) {
			return conflict("remove " + name + ": volume is in use")
		}
	}
	delete(d.st.Volumes, name)
	_ = os.Remove(d.archive(name))
	return d.save()
}

// info is the volume as the daemon reports it. The caller holds mu.
func (d *Docker) info(v *volume) docker.VolumeInfo {
	out := docker.VolumeInfo{
		Name: v.Name, Driver: v.Driver, Created: v.Created, Labels: maps.Clone(v.Labels),
		Mountpoint: "/var/lib/docker/volumes/" + v.Name + "/_data",
	}
	if fi, err := os.Stat(d.archive(v.Name)); err == nil {
		out.SizeBytes = fi.Size()
	}
	for _, c := range d.st.Containers {
		if !slices.Contains(c.Volumes, v.Name) {
			continue
		}
		if c.State == "running" {
			out.UsedBy = append(out.UsedBy, c.Name)
		} else {
			out.HeldBy = append(out.HeldBy, c.Name)
		}
	}
	return out
}

func (d *Docker) InspectVolume(ctx context.Context, name string) (docker.VolumeInfo, error) {
	if err := d.chaos.hit(ctx, "volumes", "InspectVolume"); err != nil {
		return docker.VolumeInfo{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.st.Volumes[name]
	if !ok {
		return docker.VolumeInfo{}, notFound("volume", name)
	}
	return d.info(v), nil
}

func (d *Docker) ListVolumes(ctx context.Context, labels map[string]string) ([]docker.VolumeInfo, error) {
	if err := d.chaos.hit(ctx, "volumes", "ListVolumes"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []docker.VolumeInfo
	for _, v := range d.st.Volumes {
		if matches(v.Labels, labels) {
			out = append(out, d.info(v))
		}
	}
	slices.SortFunc(out, func(a, b docker.VolumeInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (d *Docker) EnsureTool(ctx context.Context) error {
	return d.chaos.hit(ctx, "volumes", "EnsureTool")
}

// TarVolume writes the volume's archive: what the last restore put there,
// else an empty gzip tar (backup verification opens it).
func (d *Docker) TarVolume(ctx context.Context, name string, w io.Writer, _ bool) error {
	if err := d.chaos.hit(ctx, "volumes", "TarVolume"); err != nil {
		return err
	}
	if err := d.mount(name); err != nil {
		return err
	}
	f, err := os.Open(d.archive(name))
	if errors.Is(err, os.ErrNotExist) {
		return emptyTar(w)
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

func emptyTar(w io.Writer) error {
	zw := gzip.NewWriter(w)
	if err := tar.NewWriter(zw).Close(); err != nil {
		return err
	}
	return zw.Close()
}

func (d *Docker) UntarVolume(ctx context.Context, name string, r io.Reader) error {
	if err := d.chaos.hit(ctx, "volumes", "UntarVolume"); err != nil {
		return err
	}
	if err := d.mount(name); err != nil {
		return err
	}
	return writeFile(d.archive(name), r)
}

// mount is the tool container's bind: like the daemon, it creates a named
// volume nothing has mounted yet.
func (d *Docker) mount(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.st.Volumes[name]; ok {
		return nil
	}
	d.st.Volumes[name] = &volume{Name: name, Driver: "local", Created: now(), Labels: map[string]string{}}
	return d.save()
}

// ---- Images and builds.

// norm is the daemon's short form: a bare name means :latest.
func norm(ref string) string {
	if strings.Contains(ref, "@") || strings.LastIndex(ref, ":") > strings.LastIndex(ref, "/") {
		return ref
	}
	return ref + ":latest"
}

// image finds an image by id, tag or repo@digest. The caller holds mu.
func (d *Docker) image(ref string) *image {
	ref = norm(ref)
	if im, ok := d.st.Images[ref]; ok {
		return im
	}
	for _, im := range d.st.Images {
		if slices.Contains(im.Tags, ref) || slices.Contains(im.RepoDigests, ref) {
			return im
		}
	}
	return nil
}

// repo is a ref without its tag or digest.
func repo(ref string) string {
	if r, _, ok := strings.Cut(ref, "@"); ok {
		return r
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i]
	}
	return ref
}

// record adds an image, or finds the one with that digest, and tags it.
// A pull by digest leaves no tag, only the repo digest. A build has no repo
// digest. The caller holds mu.
func (d *Docker) record(ref, digest string, labels map[string]string, pulled bool) *image {
	ref = norm(ref)
	_, dg, pinned := strings.Cut(ref, "@")
	if pinned {
		digest = dg
	}
	id := hash("image:" + digest)
	im, ok := d.st.Images[id]
	if !ok {
		im = &image{ID: id, Digest: digest, Labels: maps.Clone(labels), Size: 48 << 20}
		d.st.Images[id] = im
	}
	if rd := repo(ref) + "@" + digest; pulled && !slices.Contains(im.RepoDigests, rd) {
		im.RepoDigests = append(im.RepoDigests, rd)
	}
	if !pinned {
		d.untag(ref)
		im.Tags = append(im.Tags, ref)
	}
	return im
}

// untag takes tag off whichever image has it. The caller holds mu.
func (d *Docker) untag(tag string) {
	for _, im := range d.st.Images {
		im.Tags = slices.DeleteFunc(im.Tags, func(t string) bool { return t == tag })
	}
}

func (d *Docker) Pull(ctx context.Context, ref, _ string, log io.Writer) error {
	if err := d.chaos.hit(ctx, "images", "Pull"); err != nil {
		return err
	}
	digest := hash("digest:" + norm(ref))
	lines := []string{
		ref + ": Pulling from fakehost",
		"layer0: Pull complete",
		"Digest: " + digest,
		"Status: Downloaded newer image for " + ref,
	}
	if err := d.chaos.trickle(ctx, log, lines); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.record(ref, digest, nil, true)
	return d.save()
}

func (d *Docker) LocalDigest(ctx context.Context, ref string) (string, error) {
	if err := d.chaos.hit(ctx, "images", "LocalDigest"); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	im := d.image(ref)
	if im == nil {
		return "", notFound("image", ref)
	}
	return im.Digest, nil
}

func (d *Docker) Tag(ctx context.Context, src, dst string) error {
	if err := d.chaos.hit(ctx, "images", "Tag"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	im := d.image(src)
	if im == nil {
		return notFound("image", src)
	}
	d.untag(norm(dst))
	im.Tags = append(im.Tags, norm(dst))
	return d.save()
}

func (d *Docker) RemoveImage(ctx context.Context, ref string) error {
	if err := d.chaos.hit(ctx, "images", "RemoveImage"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	im := d.image(ref)
	if im == nil {
		return notFound("image", ref)
	}
	if t := norm(ref); slices.Contains(im.Tags, t) && len(im.Tags) > 1 {
		d.untag(t) // one of several tags: untag only, as the daemon does
		return d.save()
	}
	if d.inUse(im) {
		return conflict("unable to remove image " + ref + ": image is being used by a container")
	}
	delete(d.st.Images, im.ID)
	return d.save()
}

// inUse reports whether any container runs im. The caller holds mu.
func (d *Docker) inUse(im *image) bool {
	for _, c := range d.st.Containers {
		if r := norm(c.Image); c.Image == im.ID || slices.Contains(im.Tags, r) || slices.Contains(im.RepoDigests, r) {
			return true
		}
	}
	return false
}

func (d *Docker) ListImages(ctx context.Context, labels map[string]string) ([]docker.Image, error) {
	if err := d.chaos.hit(ctx, "images", "ListImages"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []docker.Image
	for _, im := range d.st.Images {
		if matches(im.Labels, labels) {
			out = append(out, docker.Image{ID: im.ID, Tags: slices.Clone(im.Tags), Labels: maps.Clone(im.Labels), Size: im.Size})
		}
	}
	slices.SortFunc(out, func(a, b docker.Image) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (d *Docker) PruneImages(ctx context.Context, labels map[string]string, keep []string) ([]string, int64, error) {
	if err := d.chaos.hit(ctx, "images", "PruneImages"); err != nil {
		return nil, 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var removed []string
	var freed int64
	for id, im := range d.st.Images {
		if !matches(im.Labels, labels) || slices.Contains(keep, id) || d.inUse(im) ||
			slices.ContainsFunc(im.Tags, func(t string) bool { return slices.Contains(keep, t) }) {
			continue
		}
		delete(d.st.Images, id)
		removed, freed = append(removed, id), freed+im.Size
	}
	return removed, freed, d.save()
}

func (d *Docker) PruneDangling(ctx context.Context, labels map[string]string) (int, int64, error) {
	if err := d.chaos.hit(ctx, "images", "PruneDangling"); err != nil {
		return 0, 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n, freed := 0, int64(0)
	for id, im := range d.st.Images {
		if len(im.Tags) == 0 && len(im.RepoDigests) == 0 && matches(im.Labels, labels) && !d.inUse(im) {
			delete(d.st.Images, id)
			n, freed = n+1, freed+im.Size
		}
	}
	return n, freed, d.save()
}

func (d *Docker) BuildCachePrune(ctx context.Context, _ string, _ time.Duration) (int, string, error) {
	return 0, "0B", d.chaos.hit(ctx, "images", "BuildCachePrune")
}

func (d *Docker) EnsureBuilder(ctx context.Context, _ string, _ int) error {
	return d.chaos.hit(ctx, "images", "EnsureBuilder")
}

func (d *Docker) Push(ctx context.Context, _ string) error {
	return d.chaos.hit(ctx, "images", "Push")
}

func (d *Docker) Build(
	ctx context.Context,
	_, _, dockerfile, tag string,
	_, labels map[string]string,
	log io.Writer,
) (string, error) {
	if err := d.chaos.hit(ctx, "images", "Build"); err != nil {
		return "", err
	}
	lines := []string{
		"#1 [internal] load build definition from " + dockerfile,
		"#2 [internal] load metadata",
		"#3 [1/3] FROM fakehost/base",
		"#4 [2/3] COPY . .",
		"#5 [3/3] RUN make",
		"#6 exporting to image",
		"#6 naming to " + tag + " done",
	}
	if err := d.chaos.trickle(ctx, log, lines); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	im := d.record(tag, hash("build:"+tag+now()), labels, false)
	return im.ID, d.save()
}

// ---- Logs and exec.

var replicaLines = []string{"fakehost: container started", "listening on :8080", "ready"}

var runLines = []string{"fakehost: run started", "working", "done"}

// lines is what the container "printed". The caller holds mu.
func (c *ctr) lines() []string {
	if c.Labels["stackr.role"] != roleRun {
		return replicaLines
	}
	if c.State != "exited" {
		return runLines
	}
	return append(slices.Clone(runLines), fmt.Sprintf("fakehost: exited with code %d", c.ExitCode))
}

func tail(lines []string, n int) []string {
	if n >= 0 && n < len(lines) {
		return lines[len(lines)-n:]
	}
	return lines
}

func (d *Docker) Logs(ctx context.Context, id string, n int) (string, error) {
	if err := d.chaos.hit(ctx, "logs", "Logs"); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(id)
	if c == nil {
		return "", notFound("container", id)
	}
	return strings.Join(tail(c.lines(), n), "\n") + "\n", nil
}

// StreamLogs sends the lines so far, then waits while the container runs;
// the channel closes when it stops. Lines carry the real client's
// "O " stream mark and timestamp.
func (d *Docker) StreamLogs(ctx context.Context, id string, n int) (<-chan string, func(), error) {
	if err := d.chaos.hit(ctx, "logs", "StreamLogs"); err != nil {
		return nil, nil, err
	}
	d.mu.Lock()
	c := d.find(id)
	if c == nil {
		d.mu.Unlock()
		return nil, nil, notFound("container", id)
	}
	lines, done, cid := tail(c.lines(), n), d.exits[c.ID], c.ID
	d.mu.Unlock()

	ch := make(chan string, 64)
	quit := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(quit) }) }
	send := func(s string) bool {
		select {
		case ch <- "O " + now() + " " + s:
			return true
		case <-quit:
			return false
		case <-ctx.Done():
			return false
		}
	}
	go func() {
		defer close(ch)
		for _, l := range lines {
			if !send(l) || !d.chaos.pause(ctx, quit) {
				return
			}
		}
		if done == nil {
			return // not running: the stream ends with what it printed
		}
		for {
			select {
			case <-done:
				d.mu.Lock()
				var last string
				if c, ok := d.st.Containers[cid]; ok && c.Labels["stackr.role"] == roleRun {
					last = fmt.Sprintf("fakehost: exited with code %d", c.ExitCode)
				}
				d.mu.Unlock()
				if last != "" {
					send(last)
				}
				return
			case <-quit:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, stop, nil
}

func (d *Docker) Exec(ctx context.Context, _ string, _ []string) (string, error) {
	if err := d.chaos.hit(ctx, "logs", "Exec"); err != nil {
		return "", err
	}
	return "", ErrExec
}

func (d *Docker) ExecStream(ctx context.Context, _ string, _ []string, _ io.Reader) (io.Reader, func() error, error) {
	if err := d.chaos.hit(ctx, "logs", "ExecStream"); err != nil {
		return nil, nil, err
	}
	return nil, nil, ErrExec
}

// ExecTTY is an echo terminal: the panel's terminal page works, and says
// where it is.
func (d *Docker) ExecTTY(ctx context.Context, id string, _ []string) (
	io.ReadWriteCloser, func(cols, rows uint) error, func() (int, error), error,
) {
	if err := d.chaos.hit(ctx, "logs", "ExecTTY"); err != nil {
		return nil, nil, nil, err
	}
	d.mu.Lock()
	c := d.find(id)
	running := c != nil && c.running()
	d.mu.Unlock()
	switch {
	case c == nil:
		return nil, nil, nil, notFound("container", id)
	case !running:
		return nil, nil, nil, docker.ErrNotRunning
	}
	client, server := net.Pipe()
	go func() {
		defer func() { _ = server.Close() }()
		_, _ = io.WriteString(server, "fakehost: echo terminal, nothing runs here\r\n$ ")
		sc := bufio.NewScanner(server)
		sc.Split(bufio.ScanBytes)
		for sc.Scan() {
			b := sc.Bytes()
			out := b
			if b[0] == '\r' {
				out = []byte("\r\n$ ")
			}
			if _, err := server.Write(out); err != nil {
				return
			}
		}
	}()
	return client, func(uint, uint) error { return nil }, func() (int, error) { return 0, nil }, nil
}

// VIP is the iptables VIP table on the fake host: nothing to route. It
// leaves out Rebuild, Allow and Legacy, so the boot rebuild skips it.
type VIP struct{}

func (VIP) Set(context.Context, string, []string, []string) error { return nil }
func (VIP) Remove(context.Context, string) error                  { return nil }
