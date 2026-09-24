// Package dockerfake is the test double for service.Docker. It records every
// call and answers from fields a test sets; anything unset answers the zero
// value and a nil error.
package dockerfake

import (
	"context"
	"io"
	"strings"
	"sync"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// Call is one recorded call: the method and its string-ish arguments.
type Call struct {
	Method string
	Args   []string
}

func (c Call) String() string { return c.Method + "(" + strings.Join(c.Args, ", ") + ")" }

type Fake struct {
	mu    sync.Mutex
	calls []Call

	// Err makes a method fail: Err["Pull"] = errors.New("registry down").
	Err map[string]error
	// Scripted answers.
	RunID      string
	Containers []docker.Container
	Details    map[string]docker.Detail
	Volumes    []docker.VolumeInfo
	Digests    map[string]string // ref -> digest
	Members    map[string][]string
	ExecOut    string
	LogsOut    string
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
	f.calls = append(f.calls, Call{method, args})
	return f.Err[method]
}

func (f *Fake) Run(_ context.Context, s docker.ContainerSpec) (string, error) {
	return f.RunID, f.rec("Run", s.Name, s.Image)
}
func (f *Fake) Start(_ context.Context, id string) error      { return f.rec("Start", id) }
func (f *Fake) Stop(_ context.Context, id string) error       { return f.rec("Stop", id) }
func (f *Fake) Restart(_ context.Context, id string) error    { return f.rec("Restart", id) }
func (f *Fake) StopRemove(_ context.Context, id string) error { return f.rec("StopRemove", id) }
func (f *Fake) Pause(_ context.Context, id string) error      { return f.rec("Pause", id) }
func (f *Fake) Unpause(_ context.Context, id string) error    { return f.rec("Unpause", id) }

func (f *Fake) List(_ context.Context, labels map[string]string) ([]docker.Container, error) {
	var out []docker.Container
	for _, c := range f.Containers {
		if matches(c.Labels, labels) {
			out = append(out, c)
		}
	}
	return out, f.rec("List")
}

func matches(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func (f *Fake) Inspect(_ context.Context, id string) (docker.Detail, error) {
	return f.Details[id], f.rec("Inspect", id)
}

func (f *Fake) EnsureNetwork(_ context.Context, name string) error { return f.rec("EnsureNetwork", name) }
func (f *Fake) RemoveNetwork(_ context.Context, name string) error { return f.rec("RemoveNetwork", name) }
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

func (f *Fake) CreateVolume(_ context.Context, name, _ string, _ map[string]string) error {
	return f.rec("CreateVolume", name)
}
func (f *Fake) RemoveVolume(_ context.Context, name string) error { return f.rec("RemoveVolume", name) }
func (f *Fake) InspectVolume(_ context.Context, name string) (docker.VolumeInfo, error) {
	for _, v := range f.Volumes {
		if v.Name == name {
			return v, f.rec("InspectVolume", name)
		}
	}
	return docker.VolumeInfo{}, f.rec("InspectVolume", name)
}
func (f *Fake) ListVolumes(context.Context) ([]docker.VolumeInfo, error) {
	return f.Volumes, f.rec("ListVolumes")
}
func (f *Fake) TarVolume(_ context.Context, name string, _ io.Writer, _ bool) error {
	return f.rec("TarVolume", name)
}
func (f *Fake) UntarVolume(_ context.Context, name string, _ io.Reader) error {
	return f.rec("UntarVolume", name)
}

func (f *Fake) Pull(_ context.Context, ref, _ string, _ io.Writer) error { return f.rec("Pull", ref) }
func (f *Fake) LocalDigest(_ context.Context, ref string) (string, error) {
	return f.Digests[ref], f.rec("LocalDigest", ref)
}
func (f *Fake) Tag(_ context.Context, src, dst string) error    { return f.rec("Tag", src, dst) }
func (f *Fake) RemoveImage(_ context.Context, ref string) error { return f.rec("RemoveImage", ref) }
func (f *Fake) EnsureBuilder(_ context.Context, name string, _ int) error {
	return f.rec("EnsureBuilder", name)
}
func (f *Fake) Build(_ context.Context, builder, dir, dockerfile, tag string, _, _ map[string]string, _ io.Writer) error {
	return f.rec("Build", builder, dir, dockerfile, tag)
}

func (f *Fake) Logs(_ context.Context, id string, _ int) (string, error) {
	return f.LogsOut, f.rec("Logs", id)
}
func (f *Fake) StreamLogs(_ context.Context, id string, _ int) (<-chan string, func(), error) {
	ch := make(chan string)
	close(ch)
	return ch, func() {}, f.rec("StreamLogs", id)
}
func (f *Fake) Exec(_ context.Context, id string, cmd []string) (string, error) {
	return f.ExecOut, f.rec("Exec", append([]string{id}, cmd...)...)
}
func (f *Fake) ExecStream(_ context.Context, id string, cmd []string, _ io.Reader) (io.Reader, func() error, error) {
	err := f.rec("ExecStream", append([]string{id}, cmd...)...)
	return strings.NewReader(f.ExecOut), func() error { return nil }, err
}
