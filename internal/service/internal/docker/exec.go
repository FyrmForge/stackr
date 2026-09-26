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

// ExecStream runs cmd with optional stdin and streams stdout and stderr
// interleaved. The caller must call wait even on a stream it abandons: wait
// reports the exit and releases the exec.
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
		_, err := stdcopy.StdCopy(pw, io.MultiWriter(pw, &stderr), att.Reader)
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

// IsExit reports whether err is a non-zero exit, and its code.
func IsExit(err error) (ExitError, bool) {
	var e ExitError
	return e, errors.As(err, &e)
}
