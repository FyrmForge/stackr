package runtime

import "testing"

// The Move modal's bar and ETA are these numbers. rsync reports a percentage
// and not a total, so the total is derived from it, get this wrong and the
// bar either never moves or finishes early.
func TestParseRsyncProgress(t *testing.T) {
	done, total, ok := parseRsyncProgress("  1,234,567  25%   12.34MB/s    0:00:12")
	if !ok {
		t.Fatal("a normal progress line was not parsed")
	}
	if done != 1234567 {
		t.Fatalf("bytes done: %d", done)
	}
	// 25% of the total, so the total is four times what has moved.
	if total < 4900000 || total > 5000000 {
		t.Fatalf("total: %d, want about 4.94M", total)
	}

	// The first line is 0%: bytes are real, the total is not knowable yet.
	done, total, ok = parseRsyncProgress("32,768   0%    0.00kB/s    0:00:00")
	if !ok || done != 32768 || total != 0 {
		t.Fatalf("first line: done=%d total=%d ok=%v", done, total, ok)
	}

	for _, line := range []string{"", "sending incremental file list", "some/file.txt"} {
		if _, _, ok := parseRsyncProgress(line); ok {
			t.Fatalf("%q was parsed as progress", line)
		}
	}
}
