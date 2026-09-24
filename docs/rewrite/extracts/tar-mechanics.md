# Extract: volume tar mechanics

Source: `infra/runtime/runtime.go` — `volumeTool`, `toolOpts`/`toolLabels`/`toolContainer`, `volumeToolRun`, `EnsureVolumeTool`, `TarVolume`, `isTarChanged`, `UntarVolume`, `VerifyTar` (the bodies); `infra/cluster/cluster.go` + `infra/cluster/swarm.go` hold only the node-routing wrappers of the same four names.
Commit: c2423f0
Taken: the throwaway helper container (image, mount, argv, lifecycle) and the six invariants its shape is made of; the tar and untar argv; the wipe inside untar and its ordering; the `live` tolerance; archive verification.
Cut: node routing (`route`/`Self`/agent client), the volume file browser (`volumePath`, `ListVolumeFiles`, `ReadVolumeFile`, `WriteVolumeFile`, `DeleteVolumeFile`), the container move's network/label options, every Swarm call in the package.
Cuts belong to: dropped (node routing — v1 is single-node; the move), `leaf/volume` (the file browser, if it comes back at all), `flow/backup` (who freezes what, already in `backup-infra.md`).

---

## Correction: these bodies are not in `infra/cluster`

`backup-infra.md` says the tar mechanics live in `infra/cluster`. They do not.
`cluster.TarVolume`/`UntarVolume` are ten-line node routers (`route()`, then
either the local runtime or the node's agent client) and `swarm.go`'s
`VerifyTar`/`EnsureVolumeTool` are two-line passthroughs. Every mechanic below
is `infra/runtime/runtime.go`. v1 is single-node, so routing is dropped whole:

```go
// extract: dropped node routing (Cluster.route, Self, the agent leg of every
// tar/untar call), belongs nowhere — v1 is single-node.
```

## The helper container — `service/internal/docker`

Everything that touches a volume's bytes runs as a throwaway container with the
volume bind-mounted at `/data`. Not `docker run`, not a bind mount of a host
path: stackr itself usually runs containerized, so a directory *it* can write is
not a directory the *daemon* can bind-mount.

```go
// VolumeToolImage is the helper. alpine:3 because tar, gzip, rm and find are
// all it needs.
var VolumeToolImage = "alpine:3"

// volumeTool runs cmd in a throwaway container with vol mounted at /data and
// hands back the command's stdout plus a wait func that reports its exit.
//
// The caller must call wait even on a read it abandons: that is what removes
// the container.
//
// extract: docker call, becomes docker wrapper method VolumeTool
func (r *Runtime) volumeTool(ctx context.Context, vol string, ro bool, cmd []string, stdin io.Reader) (io.Reader, func() error, error) {
	// Here rather than in each caller: on a single-node install nothing ever
	// pre-pulls the image, and a box that had never pulled one answered every
	// volume op with "No such image: alpine:3". One cached ImageInspect per
	// volume op once the image is there.
	if err := r.EnsureVolumeTool(ctx); err != nil {
		return nil, nil, err
	}
	mnt := vol + ":/data"
	if ro {
		mnt += ":ro" // read-only for tar, read-write for untar
	}
	return r.toolContainer(ctx, toolOpts{
		image: VolumeToolImage, cmd: cmd, binds: []string{mnt},
		stdin: stdin, name: "stkr-vol-" + randomSuffix(),
	})
}

// volumeToolRun is volumeTool for the commands whose output is only wanted
// when they fail.
func (r *Runtime) volumeToolRun(ctx context.Context, vol string, ro bool, cmd []string, stdin io.Reader) (string, error) {
	out, wait, err := r.volumeTool(ctx, vol, ro, cmd, stdin)
	if err != nil {
		return "", err
	}
	b, _ := io.ReadAll(out)
	return string(b), wait()
}
```

`toolContainer` is the part worth copying line for line. Every comment below is
a paid-for bug.

```go
// extract: docker call, becomes docker wrapper method ToolContainer
func (r *Runtime) toolContainer(ctx context.Context, o toolOpts) (io.Reader, func() error, error) {
	stdin := o.stdin
	created, err := r.cli.ContainerCreate(ctx,
		&container.Config{
			Image: o.image,
			// (1) Cleared, not inherited. Any image whose ENTRYPOINT is a
			// binary turns `sh -c ...` into arguments to that binary, which
			// exits 1 without doing the work.
			Entrypoint:   strslice.StrSlice{},
			Cmd:          o.cmd,
			AttachStdin:  stdin != nil,
			OpenStdin:    stdin != nil,
			StdinOnce:    stdin != nil,
			AttachStdout: true,
			AttachStderr: true,
			Labels:       map[string]string{LabelManaged: "true"}, // every throwaway is stamped ours
		},
		// (2) AutoRemove off on purpose: the exit code has to be readable
		// after the process ends, and a self-removing container races
		// ContainerWait.
		&container.HostConfig{Binds: o.binds, AutoRemove: false},
		nil, nil, o.name)
	if err != nil {
		return nil, nil, err
	}
	remove := func() {
		_ = r.cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
	}

	att, err := r.cli.ContainerAttach(ctx, created.ID, container.AttachOptions{
		Stream: true, Stdin: stdin != nil, Stdout: true, Stderr: true,
	})
	if err != nil {
		remove()
		return nil, nil, err
	}

	// (3) Wait on the container BEFORE starting it: docker's own docs call the
	// other order a race, and a command that exits immediately is exactly the
	// case that loses the event.
	//
	// NextExit, not NotRunning. A container created and not yet started is
	// already "not running", so that condition is satisfied at once and
	// returns status 0 — every failure of every tool container reads as
	// success.
	okC, errC := r.cli.ContainerWait(context.WithoutCancel(ctx), created.ID, container.WaitConditionNextExit)

	if err := r.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		att.Close()
		remove()
		return nil, nil, err
	}

	if stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, stdin)
			// (4) Half-close so the command sees EOF on stdin; a full Close
			// would take the output stream down with it.
			if cw, ok := att.Conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}()
	}

	// (5) No TTY, so the attach stream is docker's multiplexed frame format
	// and has to be demuxed. stderr is buffered for the error message; stdout
	// is piped to the caller.
	pr, pw := io.Pipe()
	var stderr strings.Builder
	copyDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(pw, &stderr, att.Reader)
		_ = pw.CloseWithError(err)
		copyDone <- err
	}()

	wait := func() error {
		// (6) Close the read side FIRST. A caller that abandoned the stream, or
		// a tar whose io.Copy failed, leaves StdCopy blocked writing into the
		// pipe; waiting on copyDone before unblocking it hangs here forever,
		// holding the container and the volume open with it.
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
// extract: dropped toolOpts.network and toolOpts.labels (the two-container
// volume move, agent-to-agent rsync), belongs nowhere — v1 is single-node.
```

## Pull before the freeze — `service/internal/docker`

```go
// EnsureVolumeTool pulls the helper image if it is not on the host yet. Call
// it before freezing a container: the first TarVolume on a fresh host would
// otherwise pull inside the pause window, turning "frozen for seconds" into
// "frozen for a registry round trip".
//
// extract: docker call, becomes docker wrapper method EnsureVolumeTool
func (r *Runtime) EnsureVolumeTool(ctx context.Context) error {
	if _, err := r.cli.ImageInspect(ctx, VolumeToolImage); err == nil {
		return nil
	}
	return r.PullImage(ctx, VolumeToolImage, io.Discard)
}
```

That ordering constraint is the one this row owns. Who pauses or stops the
mounting tile, and in which mode, is `flow/backup`'s and is already written up
in `backup-infra.md` ("Volume tar"); the only place the mode reaches down into
the mechanic is the `live` flag below.

## Tar out — `service/internal/docker`

```go
// TarVolume streams a gzipped tar of the whole volume into w. Through the
// helper container's stdout rather than a shared path: see volumeTool.
//
// extract: docker call, becomes docker wrapper method TarVolume
func (r *Runtime) TarVolume(ctx context.Context, vol string, w io.Writer, live bool) error {
	out, wait, err := r.volumeTool(ctx, vol, true, // read-only mount
		[]string{"tar", "-czf", "-", "-C", "/data", "."}, nil)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, out); err != nil {
		_ = wait() // reap the container even on a failed copy
		return fmt.Errorf("tar %s: %w", vol, err)
	}
	if err := wait(); err != nil {
		if live && isTarChanged(err) {
			// The documented torn-copy mode: the caller chose not to stop the
			// tile, so a file moving under the reader is what it asked for.
			// Every other tar failure still fails the backup.
			slog.Warn("backup: files changed while copying a running tile", "volume", vol, "tar", err)
			return nil
		}
		return fmt.Errorf("tar %s: %w", vol, err)
	}
	return nil
}

// isTarChanged reports whether a tar failure is only "the file moved under
// me". busybox says "short read", GNU says "file changed as we read it"; both
// mean the archive is intact except for that file.
func isTarChanged(err error) bool {
	m := err.Error()
	return strings.Contains(m, "short read") || strings.Contains(m, "file changed as we read it")
}
```

`-C /data .` — relative members, so the archive unpacks into any volume and
carries no `/data` prefix. Gzip is done by the helper's `tar -z`, not in Go:
nothing is decompressed and recompressed on the way to the spool file.

## Wipe and untar, one container, one pass — `service/internal/docker`

```go
// UntarVolume wipes the volume and extracts the archive from src into it. The
// wipe is the destructive half of a restore; callers verify the archive first,
// because there is no way back from an emptied volume.
//
// extract: docker call, becomes docker wrapper method UntarVolume
func (r *Runtime) UntarVolume(ctx context.Context, vol string, src io.Reader) error {
	if _, err := r.volumeToolRun(ctx, vol, false, []string{ // read-write mount
		"sh", "-c", "rm -rf /data/..?* /data/.[!.]* /data/* 2>/dev/null; tar -xzf - -C /data"}, src); err != nil {
		return fmt.Errorf("restore into %s: %s", vol, strings.TrimSpace(err.Error()))
	}
	return nil
}
```

Three things in that one line, all load-bearing:

- **Three globs.** `/data/*` alone leaves dotfiles, and a database data dir is
  full of them. `..?*` and `.[!.]*` are the two dotfile shapes that are not
  `.` or `..`.
- **`;` not `&&`.** A glob that matches nothing makes `rm` exit non-zero; with
  `&&` the extract would be skipped on an already-empty volume.
- **One container, one command.** Wipe and extract cannot become two wrapper
  calls: a crash between them leaves an emptied volume and no archive in it.
  The archive arrives on the helper's stdin as it is read, so nothing is
  staged twice.

## Verifying the archive — `service/internal/flow/backup`

Not a docker call. Pure stdlib over a local file; it sat on `Cluster` only
because `Cluster` was the one door for everything.

```go
// VerifyTar reads an archive end to end and reports whether it is intact. A
// truncated or corrupt download fails here, before the volume is wiped.
func VerifyTar(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		if _, err := tr.Next(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("corrupt archive: %w", err)
		}
	}
}
```

Headers only — it walks entries without reading member bodies, so it is a
structural check, not a checksum. It still catches the case it exists for: a
truncated or half-written download.

## Dump and restore argv, and the file browser

```go
// extract: dropped managedtiles.Engine.DumpCmd/RestoreCmd — already carried
// verbatim in extracts/managedtiles.md (postgres.go: Backup/Restore).
// extract: dropped the volume file browser (volumePath, ListVolumeFiles,
// ReadVolumeFile, WriteVolumeFile, DeleteVolumeFile), belongs in leaf/volume
// if it comes back at all — same volumeTool underneath, different feature.
```

One rule from the browser family survives being dropped: paths go in as
**positional argv**, never interpolated into the script. `%q` escapes quotes and
backslashes but not `$` or backticks, so a file named `x$(...)` would have run
that command with the volume mounted writable.

## Notes for the builder

- **Targets:** `service/internal/docker` for the helper-container tar
  (`VolumeTool`, `ToolContainer`, `EnsureVolumeTool`, `TarVolume`,
  `UntarVolume`); `service/internal/flow/backup` for `VerifyTar` and for
  deciding the mode.
- **`ToolContainer` is worth being its own wrapper method**, even though tar and
  untar are its only v1 callers. The six numbered invariants are the value of
  this row, and they are properties of "run a throwaway container and read its
  exit", not of tar.
- **The `(io.Reader, func() error, error)` shape is the contract, not a style
  choice.** The caller streams stdout and must call `wait()` on every path —
  that is what reaps the container, reports the exit code and unblocks the
  demux pipe. A wrapper method that returns only `error` cannot stream a
  multi-gigabyte volume, and one that returns only a reader leaks containers.
- **The helper's name is randomized (`stkr-vol-` + suffix)** because two volume
  ops that overlap would otherwise collide on it and `ContainerCreate` would
  fail. `flow/backup`'s claim is per volume, so a run and a restore on two
  different volumes overlap freely.
- **Read-only mount for tar, read-write for untar.** One boolean, and it is the
  cheapest guard there is against a bug in the tar path writing into live data.
- **`live` is the only mode that reaches this layer.** pause and stop are
  indistinguishable from here — both just mean "nothing is writing". Keep the
  flag named for what it does (tolerate a changed file), not for the mode.
- **Verify before wipe is an ordering promise across two layers.** The wrapper
  cannot enforce it; `flow/backup` holds it (`backup-infra.md`, "Restore order
  as today", steps 4–7). A comment on `UntarVolume` saying so is what stops the
  next caller from skipping it.
- **The helper image stays a var.** The old code swapped it for a locally-built
  agent image; v1 has no agent, but it is still the knob an air-gapped install
  points at its own registry.
- **No test came over.** `runtime/livetar_test.go` (a tar of a volume being
  written) was outside this row's read scope; `live`/`isTarChanged` is the one
  branch here worth a fresh test.

Size: source 386 lines, extract 367 lines — kept Go 233, header and notes 134.
The filtered count is the Go one (224 < 386); the `.md` runs close to source
only because this row's format adds the header, the builder notes and the
"these bodies are not in `infra/cluster`" correction.
