package driver

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"tsbatch/internal/repack"
	"tsbatch/internal/sample"
)

// runTwice runs the full chain twice over the same stream and requires
// identical faults (in order), keyframe index and segment hashes.
func TestEndToEndDeterminism(t *testing.T) {
	stream := sample.Generate(sample.Options{
		Packets: 6000, PCRWrap: true, CCGapAt: 2000,
		PSIUpdateAt: 3000, TruncateBytes: 100,
	})
	run := func() *runResult {
		w := repack.New(io.Discard, repack.PolicyIsolate)
		return runStream(context.Background(), bytes.NewReader(stream), w, 4, 8, GroupPackets, 1000)
	}
	a, b := run(), run()
	if len(a.report.Faults) != len(b.report.Faults) {
		t.Fatal("fault count differs between runs")
	}
	for i := range a.report.Faults {
		if a.report.Faults[i] != b.report.Faults[i] {
			t.Fatalf("fault %d differs: %+v vs %+v", i, a.report.Faults[i], b.report.Faults[i])
		}
	}
	if len(a.report.Keyframes) != len(b.report.Keyframes) {
		t.Fatal("keyframe count differs")
	}
	for i := range a.report.Keyframes {
		if a.report.Keyframes[i] != b.report.Keyframes[i] {
			t.Fatalf("keyframe %d differs", i)
		}
	}
	// Repack to real archives and compare per-segment hashes.
	hashRun := func() []string {
		fw, err := repack.Create(filepath.Join(t.TempDir(), "a.ts"), "in.ts", repack.PolicyIsolate)
		if err != nil {
			t.Fatal(err)
		}
		runStream(context.Background(), bytes.NewReader(stream), fw.Writer, 4, 8, GroupPackets, 1000)
		if err := fw.Close(); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range fw.Segments {
			out = append(out, s.SHA256)
		}
		return out
	}
	ha, hb := hashRun(), hashRun()
	if len(ha) == 0 || len(ha) != len(hb) {
		t.Fatalf("segment hash lists differ: %d vs %d", len(ha), len(hb))
	}
	for i := range ha {
		if ha[i] != hb[i] {
			t.Fatalf("segment %d hash differs between runs", i)
		}
	}
}

func TestExitCodes(t *testing.T) {
	if code := Main([]string{"tsbatch"}); code != ExitArgs {
		t.Fatalf("no args: code=%d want %d", code, ExitArgs)
	}
	if code := Main([]string{"tsbatch", "bogus"}); code != ExitArgs {
		t.Fatalf("bad subcommand: code=%d want %d", code, ExitArgs)
	}
	if code := Main([]string{"tsbatch", "check", "/nonexistent/file.ts"}); code != ExitIO {
		t.Fatalf("missing input: code=%d want %d", code, ExitIO)
	}
	// Unrecoverable stream: sync byte destroyed.
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.ts")
	stream := sample.Generate(sample.Options{Packets: 200})
	stream[3*188] = 0x00 // break sync of packet 3
	if err := os.WriteFile(bad, stream, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Main([]string{"tsbatch", "check", bad}); code != ExitStream {
		t.Fatalf("sync loss: code=%d want %d", code, ExitStream)
	}
	// Clean stream checks out with code 0.
	good := filepath.Join(dir, "good.ts")
	if err := os.WriteFile(good, sample.Generate(sample.Options{Packets: 400}), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Main([]string{"tsbatch", "check", good}); code != ExitOK {
		t.Fatalf("clean stream: code=%d want %d", code, ExitOK)
	}
}

func TestRepackCommandAndManifest(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ts")
	out := filepath.Join(dir, "out.ts")
	if err := os.WriteFile(in, sample.Generate(sample.Options{
		Packets: 3000, CCGapAt: 900, PSIUpdateAt: 1500,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Main([]string{"tsbatch", "repack", in, out}); code != ExitOK {
		t.Fatalf("repack code=%d", code)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatal("archive not written")
	}
	hashes, err := repack.ReadManifest(out + ".mp4")
	if err != nil || len(hashes) == 0 {
		t.Fatalf("manifest: %v hashes=%d", err, len(hashes))
	}
	// Repack must refuse to overwrite its input.
	if code := Main([]string{"tsbatch", "repack", in, in}); code != ExitIO {
		t.Fatalf("overwrite input: code=%d want %d", code, ExitIO)
	}
}
