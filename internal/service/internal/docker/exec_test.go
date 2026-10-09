package docker

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"

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
