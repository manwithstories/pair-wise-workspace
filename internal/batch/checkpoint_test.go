package batch

import "testing"

func TestDigestStable(t *testing.T) {
	data := [][]byte{{1, 2, 3}, {4, 5}}
	a := DigestInput(data)
	b := DigestInput(data)
	if a != b {
		t.Error("input digest must be deterministic")
	}
	if len(a) != 64 {
		t.Errorf("digest length = %d, want 64 hex chars", len(a))
	}
}

func TestDigestSensitiveToOrder(t *testing.T) {
	d1 := DigestInput([][]byte{{1}, {2}})
	d2 := DigestInput([][]byte{{2}, {1}})
	if d1 == d2 {
		t.Error("length-prefixed digest must distinguish ordering")
	}
}

func TestParamDigestChanges(t *testing.T) {
	base := Params{Tracks: 2, Packets: 100, ReorderWindow: 64, ChunkPackets: 1024, Queue: 128, SlowWriterMs: 2, Seed: 1}
	changed := base
	changed.ReorderWindow = 32
	if DigestParams(base) == DigestParams(changed) {
		t.Error("different parameters must produce different digests")
	}
}

func TestCheckpointSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := State{InputDigest: "in", ParamDigest: "p", Segments: 3, Frames: 300, Finished: true}
	if err := Save(dir, st); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputDigest != "in" || got.Segments != 3 || !got.Finished {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestCheckpointMissingReturnsNil(t *testing.T) {
	got, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("missing checkpoint should return nil, not error")
	}
}

func TestMarkCanceledPreservesFields(t *testing.T) {
	dir := t.TempDir()
	st := State{InputDigest: "in", ParamDigest: "p", Segments: 2, Frames: 100}
	if err := MarkCanceled(dir, st); err != nil {
		t.Fatal(err)
	}
	got, _ := Load(dir)
	if !got.Canceled || got.Finished {
		t.Errorf("canceled state wrong: %+v", got)
	}
	if got.Segments != 2 {
		t.Error("committed segment count must be preserved on cancel")
	}
}
