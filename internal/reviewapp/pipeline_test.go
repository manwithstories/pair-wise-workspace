package reviewapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSuccessAndAcceptanceTokens(t *testing.T) {
	out := filepath.Join(t.TempDir(), "demo")
	rendered, code, err := Run(Config{Seed: 42, Out: out})
	if err != nil || code != ExitOK {
		t.Fatalf("Run code=%d err=%v", code, err)
	}
	for _, token := range []string{
		"PASS",
		"ROUNDTRIP=100%",
		"KEYFRAME_HIT=100%",
		"SEGMENT_HASH_STABLE=100%",
		"FIRST_BLOCKER=",
	} {
		if !strings.Contains(rendered, token) {
			t.Fatalf("render missing %q:\n%s", token, rendered)
		}
	}
	if !strings.Contains(rendered, "FIXTURES=") {
		t.Fatal("FIXTURES token missing")
	}
	// Frame artifacts exist in both export directories.
	for _, sub := range []string{"frames-a", "frames-b"} {
		entries, err := os.ReadDir(filepath.Join(out, sub))
		if err != nil || len(entries) == 0 {
			t.Fatalf("export dir %s missing or empty: %v", sub, err)
		}
	}
}

func TestRunDeterministicAcrossInvocations(t *testing.T) {
	out := filepath.Join(t.TempDir(), "demo")
	r1, c1, err := Run(Config{Seed: 7, Out: out})
	if err != nil || c1 != ExitOK {
		t.Fatalf("first run: code=%d err=%v", c1, err)
	}
	r2, c2, err := Run(Config{Seed: 7, Out: out})
	if err != nil || c2 != ExitOK {
		t.Fatalf("second run: code=%d err=%v", c2, err)
	}
	if r1 != r2 {
		t.Fatalf("reports differ across runs\n%s\n---\n%s", r1, r2)
	}
}

func TestRunRejectsRelativePath(t *testing.T) {
	_, code, err := Run(Config{Seed: 1, Out: "relative/path"})
	if code != ExitBoundary || err == nil {
		t.Fatalf("relative path: code=%d err=%v, want %d", code, err, ExitBoundary)
	}
}

func TestRunRejectsTraversalPath(t *testing.T) {
	_, code, err := Run(Config{Seed: 1, Out: "/tmp/review/../etc/escape"})
	if code != ExitBoundary || err == nil {
		t.Fatalf("traversal path: code=%d err=%v, want %d", code, err, ExitBoundary)
	}
}

func TestClassifyMapsErrorClasses(t *testing.T) {
	if c := classify(nil); c != ExitInternal {
		t.Fatalf("nil error classified %d", c)
	}
	if c := classify(sentinelBoundary()); c != ExitBoundary {
		t.Fatalf("boundary classified %d", c)
	}
	if c := classify(sentinelInternal()); c != ExitInternal {
		t.Fatalf("internal classified %d", c)
	}
}
