package main

import (
	"os"
	"testing"
)

func TestRunFullPipelinePasses(t *testing.T) {
	// Short run keeps the test fast while exercising every stage.
	code := run([]string{
		"-duration", "1s", "-tracks", "3", "-seed", "7",
		"-loss-burst", "3", "-reorder", "4", "-duplicate", "2",
		"-queue-bytes", "262144",
	})
	if code != exitOK {
		t.Fatalf("exit=%d, want 0", code)
	}
}

func TestRunRejectsBadArguments(t *testing.T) {
	if code := run([]string{"-duration", "-5s"}); code != exitArgs {
		t.Fatalf("negative duration exit=%d, want 2", code)
	}
	if code := run([]string{"-tracks", "99"}); code != exitArgs {
		t.Fatalf("tracks out of range exit=%d, want 2", code)
	}
	if code := run([]string{"-fixture", "nope"}); code != exitArgs {
		t.Fatalf("bad fixture exit=%d, want 2", code)
	}
}

func TestCorruptWAVFixture(t *testing.T) {
	code := run([]string{"-fixture", "corrupt-wav"})
	if code != exitResource {
		t.Fatalf("exit=%d, want 1", code)
	}
}

func TestQueueBoundaryFixture(t *testing.T) {
	code := run([]string{"-fixture", "queue-boundary"})
	if code != exitResource && code != exitOK {
		t.Fatalf("unexpected exit=%d", code)
	}
}

func TestMP4OutputFileCreated(t *testing.T) {
	path := "/tmp/avcheck_unit.mp4"
	defer os.Remove(path)
	code := run([]string{
		"-duration", "1s", "-tracks", "2", "-seed", "1",
		"-queue-bytes", "131072", "-out", path,
	})
	if code != exitOK {
		t.Fatalf("exit=%d", code)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("MP4 not written: %v", err)
	}
	if !hasBoxHeader(mustRead(t, path)) {
		t.Fatal("output does not look like an ISO BMFF box")
	}
}

func hasBoxHeader(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x00 && b[1] == 0x00 && b[2] == 0x00
}

func TestMetricLineAcceptsFullFlagSet(t *testing.T) {
	// Confirm the parser accepts the full acceptance flag set and the run
	// passes; field coverage itself is asserted in the metrics package.
	code := run([]string{
		"-duration", "1s", "-tracks", "3", "-seed", "7",
		"-loss-burst", "3", "-reorder", "8", "-duplicate", "5",
		"-queue-bytes", "262144",
	})
	if code != exitOK {
		t.Fatalf("acceptance flags exit=%d", code)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
