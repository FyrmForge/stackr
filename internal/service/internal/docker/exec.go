package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// Logs is the one-shot tail, stdout and stderr interleaved into one buffer.
func (d *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	rc, err := d.logs(ctx, id, tail, false)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	var buf bytes.Buffer
	_, err = stdcopy.StdCopy(&buf, &buf, rc) // no TTY: frames must be demuxed
	return buf.String(), err
}

func (d *Client) logs(ctx context.Context, id string, tail int, follow bool) (io.ReadCloser, error) {
	rc, err := d.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     follow,
		Timestamps: follow,
		Tail:       strconv.Itoa(tail),
	})
	return rc, wrap(err)
}

// StreamLogs follows with timestamps and emits one line per entry, prefixed
// "O " (stdout) or "E " (stderr). The channel closes when the container stops,
// ctx ends or stop is called.
func (d *Client) StreamLogs(ctx context.Context, id string, tail int) (<-chan string, func(), error) {
	rc, err := d.logs(ctx, id, tail, true)
	if err != nil {
		return nil, nil, err
	}
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(outW, errW, rc)
		_ = outW.CloseWithError(err)
		_ = errW.CloseWithError(err)
	}()
	ch := make(chan string, 64)
	var wg sync.WaitGroup
	done := make(chan struct{})
	scan := func(r io.Reader, mark string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20) // one long JSON line blows the default
		for sc.Scan() {
			select {
			case ch <- mark + sc.Text():
			case <-done:
				return
			}
		}
	}
	wg.Add(2)
	go scan(outR, "O ")
	go scan(errR, "E ")
	go func() {
		wg.Wait()
		close(ch)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			// rc AND both read ends: closing rc alone leaves StdCopy blocked
			// mid-write and the sibling scanner mid-scan.
			close(done)
			_ = rc.Close()
			_ = outR.Close()
			_ = errR.Close()
		})
	}
	return ch, stop, nil
}

// Exec runs cmd and returns stdout+stderr interleaved; a non-zero exit is an
// ExitError.
func (d *Client) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	execID, err := d.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", wrap(err)
	}
	att, err := d.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	_, err = stdcopy.StdCopy(&buf, &buf, att.Reader)
	att.Close()
	if err != nil {
		return buf.String(), err
	}
	return buf.String(), d.execResult(ctx, execID.ID, buf.String())
}

// execResult reads the exit of a finished exec. WithoutCancel: it has to run
// even when ctx is dead, or the exit is unreadable exactly when it matters.
func (d *Client) execResult(ctx context.Context, execID, stderr string) error {
	insp, err := d.cli.ContainerExecInspect(context.WithoutCancel(ctx), execID)
	if err != nil {
		return err
	}
	// Running means no exit yet, so ExitCode 0 is not success.
	if insp.Running {
		return ErrStillRunning
	}
	if insp.ExitCode != 0 {
		return ExitError{Code: insp.ExitCode, Stderr: strings.TrimSpace(stderr)}
	}
	return nil
}

// ExecStream runs cmd with optional stdin and streams stdout; stderr is kept
// apart for the ExitError. The caller must call wait even on a stream it
// abandons: wait reports the exit and releases the exec.
func (d *Client) ExecStream(
	ctx context.Context,
	id string,
	cmd []string,
	stdin io.Reader,
) (io.Reader, func() error, error) {
	execID, err := d.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd:          cmd,
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, nil, wrap(err)
	}
	att, err := d.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, nil, err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, stdin)
			_ = att.CloseWrite() // half-close: EOF on stdin, output stays up
		}()
	}
	pr, pw := io.Pipe()
	var stderr strings.Builder
	copyDone := make(chan struct{})
	go func() {
		err := demux(att.Reader, pw, &stderr)
		_ = pw.CloseWithError(err)
		close(copyDone)
	}()
	wait := func() error {
		// Read side FIRST: an abandoned stream leaves StdCopy blocked writing
		// into the pipe, and waiting on copyDone before unblocking it hangs.
		_ = pr.CloseWithError(io.ErrClosedPipe)
		<-copyDone
		att.Close()
		return d.execResult(ctx, execID.ID, stderr.String())
	}
	return pr, wait, nil
}

// ExecTTY runs cmd on a TTY and hands back the raw stream: no stdcopy
// demux, a TTY merges stdout and stderr. resize follows the user's window;
// exit reads the code once the stream has ended (WithoutCancel, as
// execResult); closing conn releases the exec and, if the command still
// runs, sends it SIGHUP (the closed attach stream alone leaves it behind).
// The panel has no host pid namespace, so the signal goes by a second exec
// in the container, to the pid a `sh -c 'echo $$; exec ...'` wrapper
// printed first.
func (d *Client) ExecTTY(
	ctx context.Context,
	id string,
	cmd []string,
) (conn io.ReadWriteCloser, resize func(cols, rows uint) error, exit func() (int, error), err error) {
	execID, err := d.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd:          append([]string{"sh", "-c", `echo $$; exec "$@"`, "sh"}, cmd...),
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if cerrdefs.IsConflict(err) { // the daemon's "container is not running"
		return nil, nil, nil, ErrNotRunning
	}
	if err != nil {
		return nil, nil, nil, wrap(err)
	}
	att, err := d.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{Tty: true})
	if err != nil {
		return nil, nil, nil, err
	}
	pid, err := readPid(att.Reader)
	if err != nil {
		att.Close()
		return nil, nil, nil, err
	}
	resize = func(cols, rows uint) error {
		return d.cli.ContainerExecResize(context.WithoutCancel(ctx), execID.ID,
			container.ResizeOptions{Width: cols, Height: rows})
	}
	exit = func() (int, error) {
		// The daemon may mark the exec finished a beat after the stream ends.
		for range 20 {
			insp, err := d.cli.ContainerExecInspect(context.WithoutCancel(ctx), execID.ID)
			if err != nil || !insp.Running {
				return insp.ExitCode, err
			}
			time.Sleep(50 * time.Millisecond)
		}
		return 0, ErrStillRunning
	}
	hup := func() {
		ctx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if insp, err := d.cli.ContainerExecInspect(ctx, execID.ID); err == nil && insp.Running {
			_, _ = d.Exec(ctx, id, []string{"sh", "-c", `kill -HUP "$1"`, "sh", strconv.Itoa(pid)})
		}
	}
	return &ttyConn{Reader: att.Reader, conn: att.Conn, hup: hup}, resize, exit, nil
}

// ttyConn reads through the attach's buffered reader (the pid line came off
// it) and hangs the shell up on Close.
type ttyConn struct {
	io.Reader
	conn io.WriteCloser
	hup  func()
}

func (t *ttyConn) Write(b []byte) (int, error) { return t.conn.Write(b) }
func (t *ttyConn) Close() error {
	err := t.conn.Close()
	t.hup()
	return err
}

// readPid takes the wrapper's first line, the shell's pid in the container.
func readPid(r *bufio.Reader) (int, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return 0, errors.New("the container has no sh")
	}
	return pid, nil
}

// demux splits an exec's frames: stdout to out, stderr to stderr only.
func demux(src io.Reader, out io.Writer, stderr *strings.Builder) error {
	_, err := stdcopy.StdCopy(out, stderr, src)
	return err
}

// IsExit reports whether err is a non-zero exit, and its code.
func IsExit(err error) (ExitError, bool) {
	var e ExitError
	return e, errors.As(err, &e)
}
