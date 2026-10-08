// Package dockerfake is the test double for service.Docker. It records every
// call and answers from fields a test sets; anything unset answers the zero
// value and a nil error.
package dockerfake

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// Call is one recorded call: the method and its string-ish arguments.
type Call struct {
	Method string
	Args   []string
}

func (c Call) String() string { return c.Method + "(" + strings.Join(c.Args, ", ") + ")" }

// VolumeCreate is one CreateVolume call in full.
type VolumeCreate struct {
	Name, Driver string
	Opts, Labels map[string]string
}

type Fake struct {
	mu    sync.Mutex
	calls []Call

	// Err makes a method fail: Err["Pull"] = errors.New("registry down"). A
	// key can also name one call, as Call.String does: Err["Start(new)"].
	Err map[string]error
	// Scripted answers.
	RunID         string
	RunIDs        []string               // when set, each Run takes the next one instead of RunID
	Specs         []docker.ContainerSpec // every spec Run was given
	Containers    []docker.Container
	Details       map[string]docker.Detail
	Volumes       []docker.VolumeInfo
	Created       []VolumeCreate    // every CreateVolume, with its driver, opts and labels
	Digests       map[string]string // ref -> digest
	Members       map[string][]string
	Networks      []string
	GatewayIPs    []string // what Gateways answers
	Images        []docker.Image
	Dangling      int // what PruneDangling removes
	DanglingBytes int64
	CacheRemoved  int      // what BuildCachePrune removes
	CacheTotal    string   // and the size it reports
	InUse         []string // image ids a prune cannot remove (a container uses them)
	Gone          []string // refs LocalDigest reports missing
	BuildID       string
	ExecOut       string
	TarOut        []byte    // what TarVolume writes
	Untarred      []byte    // what UntarVolume last read
	ExecIn        []byte    // what ExecStream's stdin last carried
	TTYServer     net.Conn  // the far end of the last ExecTTY pipe: the "container" side
	TTYExit       int       // what ExecTTY's exit answers
	Resizes       [][2]uint // every resize ExecTTY got, cols then rows
	LogsOut       string
	StreamOut     []string        // the lines StreamLogs sends
	ExitCode      int             // what Wait answers
	WaitBlock     bool            // Wait blocks until its ctx ends
	Host          docker.HostInfo // what HostInfo answers; zero = unknown, no ceiling
}

func New() *Fake { return &Fake{} }

// Calls returns a copy of every call so far.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

func (f *Fake) rec(method string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := Call{method, args}
	f.calls = append(f.calls, c)
	if err := f.Err[c.String()]; err != nil {
		return err
	}
	return f.Err[method]
}

func (f *Fake) Run(_ context.Context, s docker.ContainerSpec) (string, error) {
	f.mu.Lock()
	f.Specs = append(f.Specs, s)
	id := f.RunID
	if len(f.RunIDs) > 0 {
		id, f.RunIDs = f.RunIDs[0], f.RunIDs[1:]
	}
	f.mu.Unlock()
	return id, f.rec("Run", s.Name, s.Image)
}

// Create is Run without the start: it takes the next scripted id and records
// the spec like Run does.
func (f *Fake) Create(_ context.Context, s docker.ContainerSpec) (string, error) {
	f.mu.Lock()
	f.Specs = append(f.Specs, s)
	id := f.RunID
	if len(f.RunIDs) > 0 {
		id, f.RunIDs = f.RunIDs[0], f.RunIDs[1:]
	}
	f.mu.Unlock()
	return id, f.rec("Create", s.Name, s.Image)
}

func (f *Fake) HostInfo(context.Context) (docker.HostInfo, error) {
	return f.Host, f.rec("HostInfo")
}

func (f *Fake) Start(_ context.Context, id string) error {
	return f.rec("Start", id)
}

func (f *Fake) Stop(_ context.Context, id string) error {
	return f.rec("Stop", id)
}

func (f *Fake) Restart(_ context.Context, id string) error {
	return f.rec("Restart", id)
}

func (f *Fake) StopRemove(_ context.Context, id string) error {
	return f.rec("StopRemove", id)
}

func (f *Fake) Pause(_ context.Context, id string) error {
	return f.rec("Pause", id)
}

func (f *Fake) Unpause(_ context.Context, id string) error {
	return f.rec("Unpause", id)
}

func (f *Fake) List(_ context.Context, labels map[string]string) ([]docker.Container, error) {
	var out []docker.Container
	for _, c := range f.Containers {
		if matches(c.Labels, labels) {
			out = append(out, c)
		}
	}
	return out, f.rec("List")
}

// matches mirrors labelFilter: every wanted label is an exact key=value.
func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func (f *Fake) Wait(ctx context.Context, id string) (int, error) {
	if err := f.rec("Wait", id); err != nil {
		return -1, err
	}
	if f.WaitBlock {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	return f.ExitCode, nil
}

func (f *Fake) Inspect(_ context.Context, id string) (docker.Detail, error) {
	return f.Details[id], f.rec("Inspect", id)
}

func (f *Fake) EnsureNetwork(_ context.Context, name string, _ map[string]string) error {
	return f.rec("EnsureNetwork", name)
}

func (f *Fake) ListNetworks(context.Context, map[string]string) ([]string, error) {
	return f.Networks, f.rec("ListNetworks")
}

func (f *Fake) Gateways(context.Context, map[string]string) ([]string, error) {
	return f.GatewayIPs, f.rec("Gateways")
}

func (f *Fake) RemoveNetwork(_ context.Context, name string) error {
	return f.rec("RemoveNetwork", name)
}

func (f *Fake) Connect(_ context.Context, network, id string, aliases []string) error {
	return f.rec("Connect", network, id, strings.Join(aliases, ","))
}

func (f *Fake) Disconnect(_ context.Context, network, id string) error {
	return f.rec("Disconnect", network, id)
}

func (f *Fake) NetworkMembers(_ context.Context, network string) ([]string, error) {
	return f.Members[network], f.rec("NetworkMembers", network)
}

func (f *Fake) MemberAddr(_ context.Context, network, id string) (string, string, error) {
	return "", "", f.rec("MemberAddr", network, id)
}

func (f *Fake) CreateVolume(_ context.Context, name, driver string, opts, labels map[string]string) error {
	f.mu.Lock()
	f.Created = append(f.Created, VolumeCreate{name, driver, opts, labels})
	f.mu.Unlock()
	return f.rec("CreateVolume", name)
}

func (f *Fake) RemoveVolume(_ context.Context, name string) error {
	return f.rec("RemoveVolume", name)
}

func (f *Fake) InspectVolume(_ context.Context, name string) (docker.VolumeInfo, error) {
	for _, v := range f.Volumes {
		if v.Name == name {
			return v, f.rec("InspectVolume", name)
		}
	}
	return docker.VolumeInfo{}, f.rec("InspectVolume", name)
}

func (f *Fake) ListVolumes(_ context.Context, labels map[string]string) ([]docker.VolumeInfo, error) {
	var out []docker.VolumeInfo
	for _, v := range f.Volumes {
		if matches(v.Labels, labels) {
			out = append(out, v)
		}
	}
	return out, f.rec("ListVolumes")
}

func (f *Fake) EnsureTool(context.Context) error {
	return f.rec("EnsureTool")
}

func (f *Fake) TarVolume(_ context.Context, name string, w io.Writer, _ bool) error {
	if _, err := w.Write(f.TarOut); err != nil {
		return err
	}
	return f.rec("TarVolume", name)
}

func (f *Fake) UntarVolume(_ context.Context, name string, src io.Reader) error {
	b, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.Untarred = b
	f.mu.Unlock()
	return f.rec("UntarVolume", name)
}

func (f *Fake) Pull(_ context.Context, ref, _ string, _ io.Writer) error {
	return f.rec("Pull", ref)
}

func (f *Fake) LocalDigest(_ context.Context, ref string) (string, error) {
	if slices.Contains(f.Gone, ref) {
		return "", errors.Join(docker.ErrNotFound, f.rec("LocalDigest", ref))
	}
	return f.Digests[ref], f.rec("LocalDigest", ref)
}

func (f *Fake) Tag(_ context.Context, src, dst string) error {
	return f.rec("Tag", src, dst)
}

func (f *Fake) RemoveImage(_ context.Context, ref string) error {
	return f.rec("RemoveImage", ref)
}

func (f *Fake) EnsureBuilder(_ context.Context, name string, _ int) error {
	return f.rec("EnsureBuilder", name)
}

func (f *Fake) ListImages(_ context.Context, labels map[string]string) ([]docker.Image, error) {
	var out []docker.Image
	for _, im := range f.Images {
		if matches(im.Labels, labels) {
			out = append(out, im)
		}
	}
	return out, f.rec("ListImages")
}

// PruneImages drops every labelled image not in keep from Images, except
// the InUse ones. Returns the ids removed.
func (f *Fake) PruneImages(_ context.Context, labels map[string]string, keep []string) ([]string, int64, error) {
	var removed []string
	var freed int64
	var left []docker.Image
	for _, im := range f.Images {
		if !matches(im.Labels, labels) || slices.Contains(keep, im.ID) || slices.Contains(f.InUse, im.ID) ||
			slices.ContainsFunc(im.Tags, func(t string) bool { return slices.Contains(keep, t) }) {
			left = append(left, im)
			continue
		}
		removed = append(removed, im.ID)
		freed += im.Size
	}
	f.Images = left
	return removed, freed, f.rec("PruneImages", keep...)
}

// PruneDangling answers the scripted Dangling count and bytes.
func (f *Fake) PruneDangling(_ context.Context, _ map[string]string) (int, int64, error) {
	return f.Dangling, f.DanglingBytes, f.rec("PruneDangling")
}

// BuildCachePrune answers the scripted CacheRemoved count and CacheTotal.
func (f *Fake) BuildCachePrune(_ context.Context, builder string, olderThan time.Duration) (int, string, error) {
	return f.CacheRemoved, f.CacheTotal, f.rec("BuildCachePrune", builder, olderThan.String())
}

func (f *Fake) Build(
	_ context.Context,
	builder, dir, dockerfile, tag string,
	_, _ map[string]string,
	_ io.Writer,
) (string, error) {
	return f.BuildID, f.rec("Build", builder, dir, dockerfile, tag)
}

func (f *Fake) Logs(_ context.Context, id string, _ int) (string, error) {
	return f.LogsOut, f.rec("Logs", id)
}

func (f *Fake) StreamLogs(_ context.Context, id string, _ int) (<-chan string, func(), error) {
	ch := make(chan string, len(f.StreamOut))
	for _, l := range f.StreamOut {
		ch <- l
	}
	close(ch)
	return ch, func() {}, f.rec("StreamLogs", id)
}

func (f *Fake) Exec(_ context.Context, id string, cmd []string) (string, error) {
	return f.ExecOut, f.rec("Exec", append([]string{id}, cmd...)...)
}

func (f *Fake) ExecStream(
	_ context.Context,
	id string,
	cmd []string,
	stdin io.Reader,
) (io.Reader, func() error, error) {
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, nil, err
		}
		f.mu.Lock()
		f.ExecIn = b
		f.mu.Unlock()
	}
	err := f.rec("ExecStream", append([]string{id}, cmd...)...)
	return strings.NewReader(f.ExecOut), func() error { return nil }, err
}

// ExecTTY is a net.Pipe: the caller gets one end, the test the other in
// TTYServer. Resizes are recorded.
func (f *Fake) ExecTTY(_ context.Context, id string, cmd []string) (
	io.ReadWriteCloser, func(cols, rows uint) error, func() (int, error), error,
) {
	if err := f.rec("ExecTTY", append([]string{id}, cmd...)...); err != nil {
		return nil, nil, nil, err
	}
	a, b := net.Pipe()
	f.mu.Lock()
	f.TTYServer = b
	f.mu.Unlock()
	resize := func(cols, rows uint) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.Resizes = append(f.Resizes, [2]uint{cols, rows})
		return nil
	}
	return a, resize, func() (int, error) { return f.TTYExit, nil }, nil
}

// ResizeLog is a copy of the resizes ExecTTY got.
func (f *Fake) ResizeLog() [][2]uint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]uint(nil), f.Resizes...)
}
