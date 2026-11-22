package fixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	opts := DefaultOptions(filepath.Join(t.TempDir(), "batch"), 42)
	opts.Segments = 9
	return opts
}

func TestGenerateCases(t *testing.T) {
	batches, err := Generate(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != len(AllCases) {
		t.Fatalf("batches = %d", len(batches))
	}
	for _, b := range batches {
		if _, err := os.Stat(filepath.Join(b.Dir, ManifestHLS)); err != nil {
			t.Fatalf("%s: %v", b.Case, err)
		}
		if _, err := os.Stat(filepath.Join(b.Dir, ManifestDASH)); err != nil {
			t.Fatalf("%s: %v", b.Case, err)
		}
		if _, err := os.Stat(filepath.Join(b.Dir, InitName)); err != nil {
			t.Fatalf("%s: %v", b.Case, err)
		}
		if len(b.Segments) != 9 {
			t.Fatalf("%s segments = %d", b.Case, len(b.Segments))
		}
		if !strings.Contains(string(b.HLS), "#EXTM3U") || !strings.Contains(string(b.DASH), "<MPD") {
			t.Fatalf("%s manifests malformed", b.Case)
		}
	}
}

func TestCaseDamageIsPresent(t *testing.T) {
	batches, err := Generate(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	byCase := map[Case]Batch{}
	for _, b := range batches {
		byCase[b.Case] = b
	}
	normal := byCase[CaseNormal]
	for _, seg := range normal.Segments {
		if seg.Truncated || seg.Corrupt || seg.TimeJump {
			t.Fatalf("normal case damaged: %+v", seg)
		}
	}
	var trunc, corrupt, jump bool
	for _, seg := range byCase[CaseTruncated].Segments {
		trunc = trunc || seg.Truncated
	}
	for _, seg := range byCase[CaseCorruptBox].Segments {
		corrupt = corrupt || seg.Corrupt
	}
	for _, seg := range byCase[CaseTimestamp].Segments {
		jump = jump || seg.TimeJump
	}
	if !trunc || !corrupt || !jump {
		t.Fatalf("damage flags: trunc=%v corrupt=%v jump=%v", trunc, corrupt, jump)
	}
}

func TestSafeJoinRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	if _, err := SafeJoin(dir, "../evil.m4s"); err == nil {
		t.Fatal("parent escape accepted")
	}
	if _, err := SafeJoin(dir, "/etc/passwd"); err == nil {
		t.Fatal("absolute path accepted")
	}
	if _, err := SafeJoin(dir, "a/../../b"); err == nil {
		t.Fatal("nested escape accepted")
	}
	if _, err := SafeJoin(dir, ""); err == nil {
		t.Fatal("empty uri accepted")
	}
	got, err := SafeJoin(dir, "seg001.m4s")
	if err != nil || filepath.Dir(got) != dir {
		t.Fatalf("safe join = %q %v", got, err)
	}
}

func TestDeterministicOutput(t *testing.T) {
	opts := testOptions(t)
	first, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	read := func(b Batch) string {
		data, err := os.ReadFile(filepath.Join(b.Dir, "seg001.m4s"))
		if err != nil {
			t.Fatal(err)
		}
		return digest(data)
	}
	h1 := read(first[0])
	second, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != read(second[0]) {
		t.Fatal("generation is not deterministic")
	}
}

func TestRejectsUnboundedOptions(t *testing.T) {
	base := testOptions(t)
	bad := []Options{base, base, base}
	bad[0].Segments = 0
	bad[1].Segments = 100000
	bad[2].SampleRate = 44100
	for i, o := range bad {
		if _, err := Generate(o); err == nil {
			t.Fatalf("case %d accepted invalid options", i)
		}
	}
}

func TestTimestampCaseMovesOnlyOneSegment(t *testing.T) {
	opts := testOptions(t)
	batches, err := Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range batches {
		if b.Case != CaseTimestamp {
			continue
		}
		var jumps int
		for i, seg := range b.Segments {
			if seg.TimeJump {
				jumps++
				if i > 0 && seg.StartTicks <= b.Segments[i-1].StartTicks+seg.Duration-1 {
					// The jump must overlap or precede the prior span.
					if seg.StartTicks < b.Segments[i-1].StartTicks+seg.Duration {
						continue
					}
				}
			}
		}
		if jumps != 1 {
			t.Fatalf("jumps = %d", jumps)
		}
	}
}
