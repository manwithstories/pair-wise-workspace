package recovery

import (
	"os"
	"path/filepath"
	"testing"

	"tscheck/internal/fixture"
	"tscheck/internal/ts"
)

func genData(t *testing.T) []byte {
	t.Helper()
	data, _ := fixture.Generate(fixture.Options{Seed: 41, Bytes: 400_000, Branch: 0,
		Faults: fixture.FaultSpec{CCJump: true, BadPSICRC: true, Truncate: true}})
	return data
}

func TestCheckpointBoundariesAreTrusted(t *testing.T) {
	dir := t.TempDir()
	data := genData(t)
	r := Runner{Store: Store{Dir: dir}, Every: 50}
	out := filepath.Join(dir, "out.ts")
	res, err := r.Process("b0", data, out, 0)
	if err != nil || !res.Done {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	cp, ok, err := r.Store.Load("b0")
	if err != nil || !ok {
		t.Fatalf("no checkpoint: %v", err)
	}
	if cp.ByteOffset%ts.PacketSize != 0 {
		t.Fatalf("checkpoint not at packet boundary: %d", cp.ByteOffset)
	}
	trusted, _ := TrustedPackets(data)
	if cp.PacketIndex != len(trusted) {
		t.Fatalf("checkpoint index %d, trusted %d", cp.PacketIndex, len(trusted))
	}
	if cp.OutputBytes != int64(len(trusted))*ts.PacketSize {
		t.Fatalf("output bytes = %d", cp.OutputBytes)
	}
}

func TestResumeIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	data := genData(t)
	r := Runner{Store: Store{Dir: dir}, Every: 100}

	// Uninterrupted reference run in a separate store.
	refDir := t.TempDir()
	ref := Runner{Store: Store{Dir: refDir}, Every: 100}
	refOut := filepath.Join(refDir, "out.ts")
	refRes, err := ref.Process("b0", data, refOut, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Interrupted run, then resume.
	out := filepath.Join(dir, "out.ts")
	trusted, _ := TrustedPackets(data)
	crashAt := len(trusted) * 2 / 5
	part, err := r.Process("b0", data, out, crashAt)
	if err != nil {
		t.Fatal(err)
	}
	if part.Done {
		t.Fatal("interrupted run reported done")
	}
	res, err := r.Process("b0", data, out, 0)
	if err != nil || !res.Done {
		t.Fatalf("resume: %+v %v", res, err)
	}
	if res.ResumedFrom == 0 {
		t.Fatal("resume did not use checkpoint")
	}
	if res.SHA256 != refRes.SHA256 {
		t.Fatalf("resumed hash %s != reference %s", res.SHA256, refRes.SHA256)
	}
	// Re-run after completion: no-op, same hash.
	again, err := r.Process("b0", data, out, 0)
	if err != nil || again.SHA256 != res.SHA256 {
		t.Fatalf("rerun not idempotent: %+v", again)
	}
	fi, _ := os.Stat(out)
	if fi.Size() != res.OutputBytes {
		t.Fatalf("output size = %d, want %d", fi.Size(), res.OutputBytes)
	}
}
