package engine

import (
	"path/filepath"
	"testing"

	"relay/internal/index"
)

func TestCheckpointRoundTripAndInvalidation(t *testing.T) {
	dir := t.TempDir()
	st := NewCheckpointStore(filepath.Join(dir, "cp.json"))
	if _, err := st.Load(); err != ErrNoCheckpoint {
		t.Fatalf("want ErrNoCheckpoint, got %v", err)
	}
	p := index.Params{AudioRate: 48000, Channels: 2, ClockRate: 48000,
		TargetRate: 48000, TargetChans: 2}
	cp := &Checkpoint{
		InputDigest: "abc", ParamDigest: p.Digest(), EntriesDone: 256,
		OutOffset: 4096, GainDB: 9.5, Phase: "render",
	}
	if err := st.Save(cp); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.EntriesDone != 256 || got.OutOffset != 4096 || got.GainDB != 9.5 || got.Phase != "render" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if !got.Compatible("abc", p.Digest()) {
		t.Fatal("matching checkpoint must be compatible")
	}
	// 输入摘要变化即失效。
	if got.Compatible("different", p.Digest()) {
		t.Fatal("input change must invalidate")
	}
	// 参数变化即失效。
	q := p
	q.Channels = 6
	if got.Compatible("abc", q.Digest()) {
		t.Fatal("param change must invalidate")
	}
	if err := st.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(); err != ErrNoCheckpoint {
		t.Fatalf("after remove want ErrNoCheckpoint, got %v", err)
	}
}
