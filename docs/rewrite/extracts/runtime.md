# runtime

Source: `internal/stackrd/infra/runtime/` (`runtime.go`, `move.go`, `deps.go`; `service.go` and `swarm.go` left whole)
Commit: c2423f0
Taken: the plain-container path (spec → create/start/stop/remove/inspect), network calls, volume calls and the throwaway tool container, image calls (pull, push+digest, local digest, list, remove), buildx build, logs, exec.
Cut: every Swarm call (`service.go`, `swarm.go`, service logs, task labels, overlay driver), the proxyrelay/forward path, the rsync volume move, the three parsers parked here to dodge an import cycle (`ParseDevice`, `ParseFileMount`, `ParseDep`), `NormalizeRestart`, the system-role classifier, `FmtBytes`, `VerifyTar`, the domain label constants.
Cuts belong to: `service/internal/vip` (relay dial, forward listing), `service/internal/leaf/tile` (the three parsers, restart normalisation, system-role policy, the `stackr.*` labels), `service/internal/flow/deploy` (volume move, backup verify/ordering), nowhere (Swarm).

The kept code is one package, `service/internal/docker`. It takes resolved
specs and returns Docker's own words; it reads no row, decides no status,
checks no permission.

## Client and labels

```go
// Package docker is the only place the Docker SDK is imported.
type Docker struct{ cli *client.Client }

func New() (*Docker, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Docker{cli: cli}, nil
}

// LabelManaged is the only label this package owns: every container it creates
// carries it, so a sweep can find its own droppings.
const LabelManaged = "stackr.managed"

// extract: dropped LabelApp/LabelDB/LabelDeploy/LabelRun/LabelAgentTask,
// belongs in leaf/tile — they name domain rows, the wrapper takes labels as a
// map it does not read.
// extract: dropped NetworkName/PanelAlias/TraefikService/ProxyRelayImage
// consts, belongs in leaf/tile (install config) and vip (relay image).
// extract: dropped the relayMu mutex and the self-container cache, belongs in
// vip (it serialised proxyrelay create/start/connect).
```

## Networks

```go
// EnsureNetwork: NetworkList filtered by name — the filter is a SUBSTRING
// match, so compare n.Name exactly — then NetworkCreate. A concurrent create
// (two processes, or a retry) reports "already exists"; both callers wanted it,
// so that is success.
// extract: dropped Driver "overlay" + Attachable, belongs nowhere — that is the
// Swarm path and needs a manager. A v1 network is a plain bridge.
func (d *Docker) EnsureNetwork(ctx context.Context, name string) error

func (d *Docker) ListNetworks(ctx context.Context, prefix string) ([]string, error) // NetworkList, prefix in Go

// Connect joins a container to a network with optional DNS aliases (nil = a
// plain join). Already-connected is not an error: treating it as one is what
// made a proxy reattach abandon the rest of the networks halfway through, so
// every network past the first went unrouted.
// NetworkConnect with &network.EndpointSettings{Aliases: …}, nil settings when
// there are none. Swallow BOTH sentences docker uses for the same state:
// "already exists" and "already attached".
func (d *Docker) Connect(ctx context.Context, netName, containerID string, aliases []string) error

// NetworkDisconnect with force=true — a running container is the case this
// exists for.
func (d *Docker) Disconnect(ctx context.Context, netName, containerID string) error

// NetworkMembers lists the container ids on a network (NetworkInspect, walk
// n.Containers). A missing network is empty, not an error. Skip any id whose
// ContainerInspect fails: docker lists the network's own load-balancer sandbox
// among the endpoints (named "<net>-endpoint"), it is not a container, and a
// disconnect of it fails — that failure used to abort a whole drain.
func (d *Docker) NetworkMembers(ctx context.Context, name string) ([]string, error)
// extract: dropped the "is this stackr infrastructure" filter, belongs in
// leaf/tile; dropped the swarm-task-label skip and NetworkServices, Swarm only.

// MemberAddr returns a container's IP on one network plus the network's own
// subnet in CIDR form — the two things a tile behind a proxy needs to trust a
// forwarded header. Both come off one inspect: cidr = n.IPAM.Config[0].Subnet,
// ip = the part of n.Containers[id].IPv4Address before the "/" (docker suffixes
// it with the NETWORK's prefix, not a per-container one, so IPAM stays the
// authority). Not being on the network is not an error — it is the state during
// a proxy restart — so that returns an empty ip and the subnet.
func (d *Docker) MemberAddr(ctx context.Context, netName, containerID string) (ip, cidr string, err error)
```

## Images and buildx

```go
// EnsureBuilder creates a buildx docker-container builder, capped at memMB with
// a low cpu weight so a build yields to running tiles instead of pinning the
// box. The daemon's own builder ignores every resource flag, which is why this
// is a separate builder at all. Shells out: there is no buildx API.
func (d *Docker) EnsureBuilder(ctx context.Context, name string, memMB int) error {
	if exec.CommandContext(ctx, "docker", "buildx", "inspect", name).Run() == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "buildx", "create", "--name", name,
		"--driver", "docker-container",
		"--driver-opt", fmt.Sprintf("memory=%dm", memMB),
		"--driver-opt", "cpu-shares=256").CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildx create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveBuilder: `buildx rm --force`; "no builder" in the output is success.
func (d *Docker) RemoveBuilder(ctx context.Context, name string) error

// extract: dropped EnsureRemoteBuilder (remote buildkit endpoint, recreate on a
// changed address), belongs in flow/deploy if a build node ever returns.
// extract: dropped one-builder-per-org naming, belongs in leaf/tile.

// Build builds dir into tag, streaming BuildKit's plain progress to logW.
// --load puts the result in the daemon, where the push that follows finds it.
func (d *Docker) Build(ctx context.Context, builder, dir, dockerfile, tag string, buildArgs, labels map[string]string, logW io.Writer) error {
	args := []string{"build", "--builder", builder, "--load", "-f", dockerfile, "-t", tag, "--progress=plain"}
	for k, v := range buildArgs {
		args = append(args, "--build-arg", k+"="+v)
	}
	for k, v := range labels {
		args = append(args, "--label", k+"="+v)
	}
	args = append(args, ".")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, logW, logW
	return cmd.Run()
}

// Pull/Push take the credential per call, as the base64 blob the API's
// X-Registry-Auth header wants. Never `docker login`: the daemon config is
// shared by everything on the node, so logging in for one org leaves its
// credential usable by the next org's build. Blank auth = anonymous pull.
func (d *Docker) Pull(ctx context.Context, ref, auth string, logW io.Writer) error {
	rc, err := d.cli.ImagePull(ctx, ref, image.PullOptions{RegistryAuth: auth})
	if err != nil {
		return err
	}
	_, err = drainProgress(rc, logW)
	return err
}

// Push returns the manifest digest the registry stored — not the local image
// id: with the containerd image store a multi-arch image's id is its index
// while the push sends only this platform, so a ref pinned to the id names a
// manifest the registry never received.
func (d *Docker) Push(ctx context.Context, ref, auth string, logW io.Writer) (string, error) {
	rc, err := d.cli.ImagePush(ctx, ref, image.PushOptions{RegistryAuth: auth})
	if err != nil {
		return "", err
	}
	digest, err := drainProgress(rc, logW)
	if err == nil && digest == "" {
		err = fmt.Errorf("registry reported no digest for %s", ref)
	}
	return digest, err
}

type progressLine struct {
	Status      string `json:"status"`
	Progress    string `json:"progress"`
	ID          string `json:"id"`
	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// The line every push ends with, "<tag>: digest: sha256:... size: N". The one
// place both image stores report the manifest that actually went out: the
// containerd store sends no aux digest.
var pushedDigest = regexp.MustCompile(`: digest: (sha256:[0-9a-f]{64}) size: `)

// drainProgress json-decodes the stream frame by frame to EOF, printing
// "<id>: <status>" (or just the status) and skipping any frame with a
// Progress field — the layer bars repeat per layer per tick and are noise in a
// stored log. It returns the digest matched off a Status line and the error the
// STREAM reports. That error is the only signal there is: the HTTP call
// succeeds even when the push is refused, so a caller that checked only err
// recorded a failed push as a finished deployment. Keep draining after an
// errorDetail frame — the stream still has to reach EOF.
func drainProgress(rc io.ReadCloser, logW io.Writer) (digest string, streamErr error)

// LocalDigest returns the manifest digest the local image for ref was pulled
// from, "" when it was built locally or never pulled. ImageInspect, then match
// info.RepoDigests against ref with the tag stripped — strip at the LAST ":"
// only when it is after the last "/", or a registry port becomes the tag. Fall
// back to any "…@sha256:…" entry: the image may be tagged into another repo
// locally, and a pulled digest beats none.
func (d *Docker) LocalDigest(ctx context.Context, ref string) (string, error)

func (d *Docker) Tag(ctx context.Context, src, dst string) error // ImageTag
// ImageTags lists local "repo:tag" strings for a reference or a label filter:
// ImageList(filters "reference"=repo | "label"=k=v), collect im.RepoTags.
func (d *Docker) ImageTags(ctx context.Context, filter filters.Args) ([]string, error)
// RemoveImage: ImageRemove with no force, so an in-use image survives.
func (d *Docker) RemoveImage(ctx context.Context, ref string) error

// extract: dropped RegistryLogin (`docker login`), superseded by per-call auth.
// extract: dropped the CLI PullImage/PushImage variants, the SDK pair covers it.
```

## Container spec, create, lifecycle

```go
// ContainerSpec is a resolved container: every value is final, nothing here is
// looked up, defaulted from a row, or validated against a domain rule.
type ContainerSpec struct {
	Name        string
	Image       string
	Cmd         []string // override image CMD (nil = image default)
	Env         []string // KEY=VALUE
	Labels      map[string]string
	Volumes     []string          // "name-or-hostpath:/container/path"
	Ports       map[string]string // hostPort -> containerPort (published)
	Aliases     []string          // stable DNS names on the network
	NetworkName string
	CPULimit    float64 // cores, 0 = unlimited
	MemLimitMB  int     // MB, 0 = unlimited

	User          string   // "uid[:gid]" ("" = image default)
	ShmSizeMB     int      // 0 = docker default (64MB)
	Privileged    bool     // the caller gates who may set it
	Devices       []Device // already parsed
	RestartAlways bool     // else unless-stopped

	// Docker-native HEALTHCHECK, run as CMD-SHELL. Empty HealthCmd = none.
	HealthCmd          string
	HealthIntervalS    int
	HealthTimeoutS     int
	HealthRetries      int
	HealthStartPeriodS int
}

type Device struct{ Host, Container, Perms string }

// The three parsers parked in this package purely to dodge an import cycle (the
// tile service could not import the config engine, so the grammar of one line
// of one tile column was hidden behind the Docker wrapper). All three are
// leaf/tile's: they validate a tile's own columns and touch no daemon.
//
// extract: dropped ParseDevice ("host[:container[:perms]]", container path
// defaults to the host path and perms to "rwm", both must be absolute),
// belongs in leaf/tile. The wrapper takes []Device already parsed.
// extract: dropped ParseFileMount ("repo/path:/container/path[:template]" —
// source must Clean to a repo-relative path, no "..", no absolute, not ".";
// container path must be absolute), belongs in leaf/tile.
// extract: dropped ParseDep ("slug" or "slug:condition", bare = started,
// condition ∈ started|healthy|completed), belongs in leaf/tile — the waiting
// itself is flow/deploy's.
// extract: dropped RestartPolicy string + NormalizeRestart ("", "always",
// "unless-stopped" -> "", plus "on-failure"/"no"), belongs in leaf/tile.

func (d *Docker) Run(ctx context.Context, spec ContainerSpec) (string, error) {
	labels := map[string]string{LabelManaged: "true"}
	maps.Copy(labels, spec.Labels)

	exposed, bindings, err := portBindings(spec.Ports)
	if err != nil {
		return "", err
	}
	devices := make([]container.DeviceMapping, 0, len(spec.Devices))
	for _, dev := range spec.Devices {
		devices = append(devices, container.DeviceMapping{
			PathOnHost: dev.Host, PathInContainer: dev.Container, CgroupPermissions: dev.Perms,
		})
	}
	restart := container.RestartPolicyUnlessStopped
	if spec.RestartAlways {
		restart = container.RestartPolicyAlways
	}

	cfg := &container.Config{
		Image: spec.Image, Cmd: spec.Cmd, Env: spec.Env, User: spec.User,
		Labels: labels, ExposedPorts: exposed,
	}
	if spec.HealthCmd != "" {
		// Interval defaults to 5s, not docker's 30s: the deploy gate waits on
		// the first native check, and a 30s floor on every deploy is worse than
		// a slightly chattier probe.
		interval := 5
		if spec.HealthIntervalS > 0 {
			interval = spec.HealthIntervalS
		}
		cfg.Healthcheck = &container.HealthConfig{
			Test:        []string{"CMD-SHELL", spec.HealthCmd},
			Interval:    time.Duration(interval) * time.Second,
			Timeout:     time.Duration(spec.HealthTimeoutS) * time.Second,
			Retries:     spec.HealthRetries,
			StartPeriod: time.Duration(spec.HealthStartPeriodS) * time.Second,
		}
	}
	hostCfg := &container.HostConfig{
		Binds:         spec.Volumes,
		PortBindings:  bindings,
		RestartPolicy: container.RestartPolicy{Name: restart},
		Privileged:    spec.Privileged,
		ShmSize:       int64(spec.ShmSizeMB) << 20,
		Resources: container.Resources{
			NanoCPUs: int64(spec.CPULimit * 1e9),
			Memory:   int64(spec.MemLimitMB) << 20,
			Devices:  devices,
		},
	}
	netCfg := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		spec.NetworkName: {Aliases: spec.Aliases},
	}}

	resp, err := d.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, spec.Name)
	if err != nil {
		return "", err
	}
	// A container that cannot start is not left lying around under a name the
	// retry will collide with.
	if err := d.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = d.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", err
	}
	return resp.ID, nil
}

func portBindings(ports map[string]string) (nat.PortSet, nat.PortMap, error) {
	if len(ports) == 0 {
		return nil, nil, nil
	}
	exposed, bindings := nat.PortSet{}, nat.PortMap{}
	for host, cont := range ports {
		if !strings.Contains(cont, "/") {
			cont += "/tcp" // nat.Port needs the proto or the binding is ignored
		}
		p := nat.Port(cont)
		exposed[p] = struct{}{}
		bindings[p] = append(bindings[p], nat.PortBinding{HostIP: "0.0.0.0", HostPort: host})
	}
	return exposed, bindings, nil
}

// Stop/Start/Restart all pass a 10s timeout; StopRemove stops (ignoring the
// error) then force-removes. Restart keeps networks and addresses, unlike a
// recreate, so a stored proxy address stays valid. Pause is SIGSTOP via the
// cgroup — files stop changing, which is what makes a tar of the volume a
// point-in-time copy instead of a torn one.
func (d *Docker) Stop(ctx context.Context, id string) error
func (d *Docker) Start(ctx context.Context, id string) error
func (d *Docker) Restart(ctx context.Context, id string) error
func (d *Docker) StopRemove(ctx context.Context, id string) error
func (d *Docker) Pause(ctx context.Context, id string) error

// Unpause swallows "not paused": a backup that failed mid-tar still ends here.
func (d *Docker) Unpause(ctx context.Context, id string) error {
	err := d.cli.ContainerUnpause(ctx, id)
	if err != nil && strings.Contains(err.Error(), "not paused") {
		return nil
	}
	return err
}
```

## Inspect and list

```go
type Container struct {
	ID, Name, Image, State string
	// Health is docker's own word (healthy | unhealthy | starting), "" when the
	// container declares no HEALTHCHECK.
	Health string
	Labels map[string]string
	IPs    []string // addresses across all attached networks
}

// healthWord reads health off the list summary's human status string ("Up 2
// minutes (unhealthy)", "Up 5 seconds (health: starting)") — no second inspect
// per container just to colour a row.
func healthWord(status string) string // "(healthy)" | "(unhealthy)" | "(health: starting)"

// List returns every container (running or not); ListByLabel filters on
// label=value with filters.Arg("label", k+"="+v). Names come back with a
// leading "/" — trim it.
func (d *Docker) List(ctx context.Context, f filters.Args) ([]Container, error)

// extract: dropped ManagedContainer.System + systemRole/systemRoleLabel/
// ContainerIsSystem (panel | agent | proxy | registry, the containers the UI
// must not offer a stop button for), belongs in leaf/tile — it is a policy
// about what a container means, not a Docker call.
// extract: dropped Slot/TaskID/NodeID + withTask + the com.docker.swarm.* label
// constants and the slot-ordered sort, Swarm only.

// HealthStatus is the live read (inspect) rather than the list summary:
// info.State.Health.Status, "" when State or Health is nil.
func (d *Docker) HealthStatus(ctx context.Context, id string) (string, error)
func (d *Docker) IsRunning(ctx context.Context, id string) (bool, error) // State != nil && State.Running

// Detail is the curated inspect the UI shows: ID, Name, Image, State,
// StartedAt, ports as "hostIP:hostPort -> containerPort/proto", mounts as
// "source -> destination", network names.
type Detail struct{ ID, Name, Image, State, Started string; Ports, Mounts, Networks []string }
func (d *Docker) Inspect(ctx context.Context, id string) (*Detail, error)

// SelfID is the container this process runs in, "" when it is not in one.
// The hostname is the usual answer and is wrong here: the panel runs with
// --hostname set so its network alias resolves, and a lookup by that name finds
// nothing — silently, which is how it ended up thinking it had no identity at
// all. So: try the hostname, then read the id out of the binds docker gives
// every container. Cache it, it cannot change while the process lives.
var containerIDRe = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

func (d *Docker) SelfID(ctx context.Context) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		if _, err := d.cli.ContainerInspect(ctx, h); err == nil {
			return h
		}
	}
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	if m := containerIDRe.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}
```

## Logs

```go
// logs opens the stream. The Go client, not `docker logs`: this runs in images
// that ship no docker CLI.
func (d *Docker) logs(ctx context.Context, id string, tail int, follow, timestamps bool) (io.ReadCloser, error) {
	return d.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: follow,
		Timestamps: timestamps, Tail: strconv.Itoa(tail),
	})
}

// Logs is the one-shot tail. There is no TTY, so the stream is docker's
// multiplexed frame format and MUST go through stdcopy; both streams land in
// one buffer because a human reads them interleaved, as the CLI printed them.
func (d *Docker) Logs(ctx context.Context, id string, tail int) (string, error) {
	rc, err := d.logs(ctx, id, tail, false, false)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	_, err = stdcopy.StdCopy(&buf, &buf, rc)
	return buf.String(), err
}

// StreamLogs follows with timestamps and emits one line per entry, prefixed
// "O " (stdout) or "E " (stderr) so a viewer can colour the streams. The
// channel closes when the container stops or ctx ends.
// Shape: StdCopy demuxes into TWO io.Pipes so each stream keeps its own mark,
// one scanner goroutine per pipe pushes "O "/"E " lines into a buffered chan,
// a third waits on both and closes it.
//   - stop() closes rc AND both READ ends. Closing rc alone leaves StdCopy
//     blocked mid-Write and the sibling scanner mid-Scan: every closed log tab
//     leaked three goroutines and two pipes.
//   - sc.Buffer(64KB, 1MB) — one long JSON log line blows the default.
func (d *Docker) StreamLogs(ctx context.Context, id string, tail int) (<-chan string, func(), error)

// extract: dropped streamMarked (the `docker service logs` CLI twin), Swarm only.
```

## Exec

```go
// ExecStream runs cmd in the container with optional stdin and streams stdout
// to the reader. The caller must call wait even on a stream it abandons: wait
// is what reports the exit and releases the exec.
func (d *Docker) ExecStream(ctx context.Context, id string, cmd []string, stdin io.Reader) (io.Reader, func() error, error) {
	execID, err := d.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: cmd, AttachStdin: stdin != nil, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return nil, nil, err
	}
	att, err := d.cli.ContainerExecAttach(ctx, execID.ID, container.ExecStartOptions{})
	if err != nil {
		return nil, nil, err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, stdin)
			// Half-close so the command sees EOF; a full Close takes the output
			// stream down with it.
			if cw, ok := att.Conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}()
	}
	pr, pw := io.Pipe()
	var stderr strings.Builder
	copyDone := make(chan struct{})
	go func() {
		_, err := stdcopy.StdCopy(pw, &stderr, att.Reader)
		_ = pw.CloseWithError(err)
		close(copyDone)
	}()
	wait := func() error {
		// Close the read side FIRST. A caller that abandoned the stream leaves
		// StdCopy blocked writing into the pipe; waiting on copyDone before
		// unblocking it hangs here forever, holding the exec open with it.
		_ = pr.CloseWithError(io.ErrClosedPipe)
		<-copyDone
		att.Close()
		// WithoutCancel: the inspect has to run even when ctx is already dead,
		// or the exit code is unreadable exactly when it matters.
		insp, err := d.cli.ContainerExecInspect(context.WithoutCancel(ctx), execID.ID)
		if err != nil {
			return err
		}
		// Running means the exec has not finished, so ExitCode is 0 because
		// there is no exit yet, not because it succeeded. Reading it as success
		// is how an interrupted database dump came back green.
		if insp.Running {
			return fmt.Errorf("the command was still running when its output ended: %s", strings.TrimSpace(stderr.String()))
		}
		if insp.ExitCode != 0 {
			return fmt.Errorf("exit status %d: %s", insp.ExitCode, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	return pr, wait, nil
}

// ExecTTY opens an interactive TTY (nil cmd = a login shell). With Tty: true
// the stream is RAW — no stdcopy. Returns the conn, a resize func
// (ContainerExecResize) and the closer.
func (d *Docker) ExecTTY(ctx context.Context, id string, cmd []string) (io.ReadWriter, func(w, h uint) error, func(), error)
// default cmd: []string{"sh", "-c", "command -v bash >/dev/null && exec bash || exec sh"}

// Exec runs cmd and returns stdout+stderr interleaved into one buffer, the way
// `docker exec` printed them; non-zero exit is an error carrying the code.
func (d *Docker) Exec(ctx context.Context, id string, cmd []string) (string, error)

// ExecShell runs a shell command. On a ctx timeout the process GROUP is killed
// inside the container too: killing the client alone leaves the command
// running. The pid is parked in a file because that is the only handle the
// second exec has on the first.
// Runs `echo $$ > <pidfile>; <command>`; on ctx.Err() a SECOND exec, on a fresh
// context (the timed-out one cannot carry the kill), runs
// `kill -9 -$(cat f); kill -9 $(cat f); rm -f f` — the group first, then the
// leader. Clean runs just remove the pidfile.
func (d *Docker) ExecShell(ctx context.Context, id, command string) (string, error)

func randomSuffix() string { /* 6 random bytes, hex */ }
```

## Volumes

```go
type VolumeInfo struct {
	Name, Driver, Created, Mountpoint string
	SizeBytes                         int64    // -1 = unknown
	UsedBy                            []string // running containers mounting it
	// HeldBy is containers that mount it but are NOT running. Docker refuses to
	// delete a volume while either list is non-empty, so these are named
	// separately: a volume held only by a corpse is free to delete, one held by
	// a stopped tile is that tile's data.
	HeldBy []string
}

// Inspect/List fill SizeBytes from DiskUsage(types.VolumeObject) — the only API
// that reports a volume's size, and a full scan. Click-time, never polled.
// UsedBy/HeldBy come from one ContainerList(All) walking c.Mounts for
// m.Type == "volume".
func (d *Docker) InspectVolume(ctx context.Context, name string) (VolumeInfo, error)
func (d *Docker) ListVolumes(ctx context.Context) ([]VolumeInfo, error)

// CreateVolume is a no-op if the volume exists — and NOT idempotent across
// differing opts: docker returns the existing volume unchanged, so an edit is
// remove-then-recreate. driver/opts is how a storage sub-path becomes an
// nfs/cifs/bind mount; both empty gives the local driver.
func (d *Docker) CreateVolume(ctx context.Context, name, driver string, opts map[string]string) error {
	_, err := d.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Driver: driver, DriverOpts: opts})
	return err
}

func (d *Docker) RemoveVolume(ctx context.Context, name string) error // VolumeRemove(force=false)

// PurgeVolume removes the volume after clearing the STOPPED containers holding
// it. Docker refuses to delete a volume any container references, so a volume
// whose old container is still in the list is otherwise permanently undeletable.
// Running containers are skipped, so this can never take down something that is
// serving — and docker's own refusal is still the safety net.
func (d *Docker) PurgeVolume(ctx context.Context, name string) error

// VolumeSize is du inside the tool container, for showing a real number before
// a copy: volumeToolRun("du", "-sb", "/data"), first field.
func (d *Docker) VolumeSize(ctx context.Context, vol string) (int64, error)
```

### The throwaway tool container

Every volume file op mounts the volume into a throwaway container, so it works
the same whether the tile using the volume is running or stopped and never
depends on what binaries the tile's image ships.

```go
// ToolImage is the throwaway container's image. alpine on a normal host; a node
// with no reason to have alpine points it at an image it already runs, because
// a volume browse is not the moment to pull one.
var ToolImage = "alpine:3"

type toolOpts struct {
	image   string
	cmd     []string
	binds   []string
	stdin   io.Reader
	name    string
	network string           // only ops whose containers talk to each other need one
	labels  map[string]string
}

// toolContainer starts a throwaway container and hands back its stdout plus a
// wait func reporting its exit. The caller MUST call wait even on a read it
// abandons: wait is what removes the container.
func (d *Docker) toolContainer(ctx context.Context, o toolOpts) (io.Reader, func() error, error) {
	labels := map[string]string{LabelManaged: "true"}
	maps.Copy(labels, o.labels)
	var netCfg *network.NetworkingConfig
	if o.network != "" {
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{o.network: {}}}
	}
	created, err := d.cli.ContainerCreate(ctx,
		&container.Config{
			Image: o.image,
			// CLEARED, not inherited. If the tool image is one of ours its
			// ENTRYPOINT is our own binary, and leaving it turns `sh -c ...`
			// into arguments to that binary, which exits 1 having done nothing.
			Entrypoint:   strslice.StrSlice{},
			Cmd:          o.cmd,
			AttachStdin:  o.stdin != nil,
			OpenStdin:    o.stdin != nil,
			StdinOnce:    o.stdin != nil,
			AttachStdout: true,
			AttachStderr: true,
			Labels:       labels,
		},
		&container.HostConfig{Binds: o.binds, AutoRemove: false},
		netCfg, nil, o.name)
	if err != nil {
		return nil, nil, err
	}
	// AutoRemove is off on purpose: the exit code has to be readable after the
	// process ends, and a self-removing container races ContainerWait.
	remove := func() {
		_ = d.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}
	att, err := d.cli.ContainerAttach(ctx, created.ID, container.AttachOptions{
		Stream: true, Stdin: o.stdin != nil, Stdout: true, Stderr: true,
	})
	if err != nil {
		remove()
		return nil, nil, err
	}
	// Wait BEFORE start — docker's own docs call the other order a race, and a
	// command that exits immediately is exactly the case that loses the event.
	// NextExit, not NotRunning: a created-but-unstarted container is already
	// "not running", so NotRunning returns status 0 at once and every failure
	// of every tool container reads as success.
	okC, errC := d.cli.ContainerWait(context.WithoutCancel(ctx), created.ID, container.WaitConditionNextExit)

	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		att.Close()
		remove()
		return nil, nil, err
	}
	if o.stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, o.stdin)
			if cw, ok := att.Conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite() // half-close: EOF on stdin, output stays up
			}
		}()
	}
	pr, pw := io.Pipe()
	var stderr strings.Builder
	copyDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(pw, &stderr, att.Reader)
		_ = pw.CloseWithError(err)
		copyDone <- err
	}()
	wait := func() error {
		// Read side first, same reason as ExecStream: an abandoned stream
		// otherwise wedges StdCopy and this wait, holding the volume open.
		_ = pr.CloseWithError(io.ErrClosedPipe)
		<-copyDone
		att.Close()
		defer remove()
		select {
		case err := <-errC:
			return err
		case st := <-okC:
			if st.Error != nil && st.Error.Message != "" {
				return fmt.Errorf("%s", st.Error.Message)
			}
			if st.StatusCode != 0 {
				msg := strings.TrimSpace(stderr.String())
				if msg == "" {
					msg = fmt.Sprintf("exit status %d", st.StatusCode)
				}
				return fmt.Errorf("%s", msg)
			}
			return nil
		}
	}
	return pr, wait, nil
}

// volumeTool mounts vol at /data in a tool container. EnsureTool is called here
// rather than in each caller: on a host that had never pulled alpine every
// volume size, listing and browse answered "No such image: alpine:3", because
// only two of the callers remembered to pull it.
func (d *Docker) volumeTool(ctx context.Context, vol string, ro bool, cmd []string, stdin io.Reader) (io.Reader, func() error, error) {
	if err := d.EnsureTool(ctx); err != nil {
		return nil, nil, err
	}
	mnt := vol + ":/data"
	if ro {
		mnt += ":ro"
	}
	return d.toolContainer(ctx, toolOpts{image: ToolImage, cmd: cmd, binds: []string{mnt},
		stdin: stdin, name: "stkr-vol-" + randomSuffix()})
}

// volumeToolRun is volumeTool for commands whose output is only wanted on
// failure: read all, then wait.
func (d *Docker) volumeToolRun(ctx context.Context, vol string, ro bool, cmd []string, stdin io.Reader) (string, error)

// EnsureTool pulls the helper image if it is not on the host. Call it before
// freezing a container: the first tar on a fresh host would otherwise pull
// inside the pause window, turning "frozen for seconds" into "frozen for a
// registry round trip". ImageInspect, then Pull.
func (d *Docker) EnsureTool(ctx context.Context) error

// volumePath confines a user-supplied path to the mount: Clean on a /-rooted
// path squeezes out every "..", so the result is always under /data.
func volumePath(p string) string { return path.Join("/data", path.Clean("/"+p)) }
```

### Volume file ops

```go
type VolumeEntry struct {
	Name    string
	Dir     bool
	Size    int64
	ModTime time.Time
}

// ListVolumeFiles lists one level, directories first. find+stat because busybox
// ls has no stable machine format:
//
//	find <path> -maxdepth 1 -mindepth 1 -exec stat -c "%F|%s|%Y|%n" {} +
//
// Split each line into 4 on "|" (type|size|mtime|name) and skip anything else:
// a filename holding "\n" corrupts its own row and nothing else's.
func (d *Docker) ListVolumeFiles(ctx context.Context, vol, dir string) ([]VolumeEntry, error)

// ReadVolumeFile: cat, returned as a ReadCloser plus the wait func.
func (d *Docker) ReadVolumeFile(ctx context.Context, vol, file string) (io.ReadCloser, func() error, error)

// WriteVolumeFile streams src into the volume, creating parents. The paths go
// in as POSITIONAL ARGUMENTS, never interpolated: Go's %q escapes quotes and
// backslashes but not $ or backticks, both live inside shell double quotes, so
// an uploaded file named `x$(...)` would have run that command with the volume
// mounted writable.
func (d *Docker) WriteVolumeFile(ctx context.Context, vol, file string, src io.Reader) error {
	dst := volumePath(file)
	_, err := d.volumeToolRun(ctx, vol, false, []string{
		"sh", "-c", `mkdir -p "$1" && cat > "$2"`, "sh", path.Dir(dst), dst}, src)
	return err
}

// DeleteVolumeFile: rm -rf, refusing "/data" itself.
func (d *Docker) DeleteVolumeFile(ctx context.Context, vol, file string) error

// TarVolume streams a gzipped tar of the whole volume into w. Through the
// helper's stdout rather than a shared path on purpose: this process usually
// runs containerized, so a directory it can write is not a directory the daemon
// can bind-mount. live = the caller chose not to stop the tile, so a file
// moving under the reader is what it asked for; every other tar failure still
// fails the copy.
//	volumeTool(vol, ro, []string{"tar", "-czf", "-", "-C", "/data", "."})
//	io.Copy(w, out)  // on error still call wait(), or the container leaks
//	wait()           // tolerated only when live && isTarChanged(err)
//
// isTarChanged: busybox says "short read", GNU says "file changed as we read
// it"; both mean the archive is intact except for that one file.
func (d *Docker) TarVolume(ctx context.Context, vol string, w io.Writer, live bool) error

// UntarVolume wipes the volume and extracts src into it. The glob trio catches
// dotfiles, which `/data/*` alone misses.
func (d *Docker) UntarVolume(ctx context.Context, vol string, src io.Reader) error {
	_, err := d.volumeToolRun(ctx, vol, false, []string{
		"sh", "-c", "rm -rf /data/..?* /data/.[!.]* /data/* 2>/dev/null; tar -xzf - -C /data"}, src)
	return err
}
// extract: dropped VerifyTar (gzip+tar read-through before the wipe) — no
// Docker in it, and the "verify before you destroy" ordering is the rule, not
// the call: belongs in flow/deploy (restore).
```

## Host info, stats, prune

```go
type HostInfo struct {
	ServerVersion, OS, Arch          string
	NCPU                             int
	MemTotal                         int64
	Containers, ContainersRunning, Images int
}

func (d *Docker) Info(ctx context.Context) (HostInfo, error) // straight off cli.Info

// Stats is one instantaneous reading. Rx/Tx are cumulative since start.
type Stats struct {
	CPUPct   float64
	MemBytes uint64
	RxBytes, TxBytes uint64
}

// ContainerStatsOneShot (no stream to drain), json-decode into
// container.StatsResponse. Docker reports totals, not a percentage:
//
//	cpuDelta := CPUStats.CPUUsage.TotalUsage - PreCPUStats.CPUUsage.TotalUsage
//	sysDelta := CPUStats.SystemUsage - PreCPUStats.SystemUsage
//	pct = cpuDelta / sysDelta * OnlineCPUs * 100   // only when sysDelta > 0
//
// Mem is MemoryStats.Usage; Rx/Tx are summed over every entry in .Networks.
func (d *Docker) Stats(ctx context.Context, id string) (Stats, error)

// Prune is the three API calls behind `docker system prune -f`:
// ContainersPrune, ImagesPrune filtered dangling=true (a TAGGED image is
// somebody's rollback target, never prune those), BuildCachePrune. Sum
// SpaceReclaimed across all three.
func (d *Docker) Prune(ctx context.Context) (reclaimed int64, summary string, err error)
// extract: dropped FmtBytes ("1.2 GB"), belongs in the web layer.
```

## Notes for the builder

- One package owns `client.Client`. Everything above takes resolved values and
  returns Docker's words; nothing reads a row or decides a tile's status.
- `stdcopy.StdCopy` is mandatory on every non-TTY attach/exec/log stream —
  raw reads give you framed bytes. With `Tty: true` it is the opposite: raw,
  no stdcopy.
- The pipe deadlock is the bug that recurs: on any wait/close path, close the
  **read** end (`pr.CloseWithError`) before waiting on the copy goroutine.
  Closing only the docker-side ReadCloser leaves StdCopy blocked on a write.
- `ContainerWait` goes **before** `ContainerStart`, with
  `WaitConditionNextExit` — `NotRunning` is already true for a created
  container and returns status 0 immediately.
- `AutoRemove: false` on anything whose exit code you read; AutoRemove races
  the wait.
- Wrap post-cancel cleanup in `context.WithoutCancel`: exec inspect, container
  remove, the kill after a timeout.
- Exec exit codes: check `insp.Running` first. A still-running exec reports
  `ExitCode: 0`.
- Health: two sources. The list summary carries `"(healthy)"` in its status
  string (free); `inspect` carries `State.Health.Status` (a call per
  container). No HEALTHCHECK means `""`, not "unhealthy".
- Healthcheck interval defaults to 5s here, not Docker's 30s, because the
  deploy gate waits on the first check.
- A push's HTTP call succeeds even when the push is refused. The error lives in
  the JSON progress stream (`errorDetail`), and the digest lives in the
  `": digest: sha256:… size: "` line — not in the local image id.
- Registry credentials go per call as `RegistryAuth`, never `docker login`: the
  daemon config is shared by every tenant on the node.
- buildx has no API — shell out. Only the `docker-container` driver honours the
  memory/cpu caps.
- Tool containers must clear `Entrypoint` (`strslice.StrSlice{}`) when the image
  might be one of ours.
- Volume paths from users go through `path.Join("/data", path.Clean("/"+p))`
  and into the command as **argv**, never interpolated into `sh -c`.
- `nat.Port` needs a `/tcp` suffix or the binding is silently ignored.
- Volume sizes only come from `DiskUsage` — a full scan, so click-time only.
- Docker refuses to remove a volume any container references, running or not;
  clear the stopped ones first.
- "already exists" / "already attached" / "not found" / "not paused" are
  string-matched. There are no typed errors for these in the SDK.

Size: source 2395 lines, extract 884 lines
