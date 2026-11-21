package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run executes one command line and returns stdout, stderr and the process
// exit code the binary would produce.
func run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := Run(args, &out, &errOut)
	code := 0
	if err != nil {
		if e, ok := err.(*ExitError); ok {
			code = e.Code
			if e.Message != "" {
				errOut.WriteString(e.Message)
			}
		} else {
			code = 3
			errOut.WriteString(err.Error())
		}
	}
	return out.String(), errOut.String(), code
}

func TestDemoMatchesAcceptanceLine(t *testing.T) {
	dir := t.TempDir()
	out, errOut, code := run(t, "demo", "--seed", "17", "--queue", "4", "--slow-consumer-ms", "2", "--keep", dir)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	want := "roundtrip_match=100.00% keyframe_hit=100.00% segment_hash_match=100.00% peak_queue=4"
	if !strings.Contains(out, want) {
		t.Fatalf("missing acceptance line\nwant: %s\ngot:\n%s", want, out)
	}
	if !strings.Contains(out, "stable=true") {
		t.Fatalf("mapping digest is not deterministic:\n%s", out)
	}
	// The demo must leave real output files behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var mp4s, mkvs int
	for _, e := range entries {
		switch filepath.Ext(e.Name()) {
		case ".mp4":
			mp4s++
		case ".mkv":
			mkvs++
		}
	}
	if mp4s != 2 || mkvs != 2 {
		t.Fatalf("demo kept %d mp4 and %d mkv files", mp4s, mkvs)
	}
}

func TestDemoRejectsNonPositiveQueue(t *testing.T) {
	if _, _, code := run(t, "demo", "--queue", "0"); code != 3 {
		t.Fatalf("exit=%d", code)
	}
}

func TestVerifyTruncatedFails(t *testing.T) {
	out, errOut, code := run(t, "verify", "--fixture", "truncated", "--recover=false")
	if code != 2 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "status=error stage=parse offset=") || !strings.Contains(errOut, "exit=2") {
		t.Fatalf("stage line missing:\n%s", errOut)
	}
	if strings.Contains(out, "roundtrip_match") {
		t.Fatalf("a failed verify must not print fidelity rates:\n%s", out)
	}
}

func TestRecoverTruncatedSucceeds(t *testing.T) {
	out, errOut, code := run(t, "recover", "--fixture", "truncated", "--recover=true")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"status=recovered", "skipped_segments=1", "source_offset=", "hash_verified_segments=", "exit=0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hash_verified_segments=0") {
		t.Fatalf("no segment was hash-verified:\n%s", out)
	}
}

func TestOversizedIsFatalEvenWithRecovery(t *testing.T) {
	_, errOut, code := run(t, "recover", "--fixture", "oversized", "--recover=true")
	if code != 2 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "status=error stage=parse") {
		t.Fatalf("missing stage line:\n%s", errOut)
	}
	if strings.Contains(errOut, "status=recovered") {
		t.Fatal("ambiguous boundary must not be reported as recovered")
	}
}

func TestBadBlockIsFatalWithoutRecovery(t *testing.T) {
	_, errOut, code := run(t, "verify", "--fixture", "badblock", "--recover=false")
	if code != 2 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(errOut, "stage=demux") && !strings.Contains(errOut, "stage=parse") {
		t.Fatalf("expected a parse or demux stage:\n%s", errOut)
	}
}

func TestCleanFixturesAllPass(t *testing.T) {
	for _, kind := range []string{"clean", "unknown-size", "no-cues", "out-of-order", "slow-consumer"} {
		out, errOut, code := run(t, "verify", "--fixture", kind, "--recover=false")
		if code != 0 {
			t.Fatalf("%s: exit=%d stderr=%s", kind, code, errOut)
		}
		if !strings.Contains(out, "roundtrip_match=100.00%") {
			t.Fatalf("%s: not full fidelity:\n%s", kind, out)
		}
	}
}

func TestInspectRejectsEscape(t *testing.T) {
	_, errOut, code := run(t, "inspect", "--input", "../escape.mkv")
	if code != 2 && code != 3 {
		t.Fatalf("exit=%d stderr=%s", code, errOut)
	}
}

func TestUnknownFixtureIsRuntimeError(t *testing.T) {
	if _, _, code := run(t, "verify", "--fixture", "nope"); code != 3 {
		t.Fatalf("exit=%d", code)
	}
}

func TestNoArgs(t *testing.T) {
	if _, _, code := run(t); code != 3 {
		t.Fatalf("exit=%d", code)
	}
}

var _ io.Writer = (*bytes.Buffer)(nil)
