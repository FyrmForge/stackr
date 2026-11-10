package docker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/pkg/stdcopy"
)

// A dump stream is stdout alone: a warning on stderr (pg_dumpall prints
// some) must not land inside the archive, where it restores as a syntax
// error. It is kept for the ExitError.
func TestDemuxKeepsStderrOutOfTheStream(t *testing.T) {
	var framed bytes.Buffer
	_, _ = stdcopy.NewStdWriter(&framed, stdcopy.Stdout).Write([]byte("SELECT 1;\n"))
	_, _ = stdcopy.NewStdWriter(&framed, stdcopy.Stderr).Write([]byte("pg_dumpall: warning\n"))
	_, _ = stdcopy.NewStdWriter(&framed, stdcopy.Stdout).Write([]byte("SELECT 2;\n"))

	var out bytes.Buffer
	var stderr strings.Builder
	if err := demux(&framed, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "SELECT 1;\nSELECT 2;\n" {
		t.Errorf("stream = %q, want stdout only", got)
	}
	if got := stderr.String(); got != "pg_dumpall: warning\n" {
		t.Errorf("stderr = %q", got)
	}
}

// A user's exec (MergeStderr) sees stderr in order with stdout, and the
// ExitError still gets it.
func TestMergeStderrInterleaves(t *testing.T) {
	var framed bytes.Buffer
	_, _ = stdcopy.NewStdWriter(&framed, stdcopy.Stdout).Write([]byte("a\n"))
	_, _ = stdcopy.NewStdWriter(&framed, stdcopy.Stderr).Write([]byte("Did not find any relations.\n"))
	_, _ = stdcopy.NewStdWriter(&framed, stdcopy.Stdout).Write([]byte("b\n"))

	if MergeStderr(context.Background()).Value(mergeKey{}) == nil {
		t.Fatal("MergeStderr set nothing")
	}
	var out bytes.Buffer
	var stderr strings.Builder
	if err := demux(&framed, &out, io.MultiWriter(&stderr, &out)); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "a\nDid not find any relations.\nb\n" {
		t.Errorf("stream = %q", got)
	}
	if stderr.String() != "Did not find any relations.\n" {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// The wrapper's first line is the shell's pid; the rest of the stream stays
// for the terminal.
func TestReadPidLeavesTheStream(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("42\r\n$ "))
	pid, err := readPid(r)
	if err != nil || pid != 42 {
		t.Fatalf("pid = %d, %v, want 42", pid, err)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "$ " {
		t.Errorf("rest = %q", rest)
	}
}

// An image without sh answers the wrapper with the OCI error text.
func TestReadPidNoSh(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("OCI runtime exec failed: exec: \"sh\": not found\r\n"))
	if _, err := readPid(r); err == nil || !strings.Contains(err.Error(), "no sh") {
		t.Fatalf("err = %v, want the no sh error", err)
	}
}

// A wedged exec (the connection never delivers) returns when ctx ends.
func TestCopyUntilDoneCancels(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- copyUntilDone(ctx, &buf, pr, func() { _ = pr.Close() }) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want deadline exceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("copy still blocked after ctx ended")
	}
}
