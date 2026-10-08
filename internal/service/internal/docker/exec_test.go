package docker

import (
	"bytes"
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
