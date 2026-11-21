package verify

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/archive/rtprestore/internal/checkpoint"
)

func params() checkpoint.Params {
	return checkpoint.Params{Seed: 41, LossPercent: 3, ReorderPercent: 7, WrapAt: 4294967000,
		DriftPPM: 180, VFR: true, CheckpointEvery: 64}
}

func seedStore(t *testing.T, dir string, n int) checkpoint.Store {
	t.Helper()
	s, err := checkpoint.Open(dir, 64, params())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := s.WriteShard(i, bytes.NewReader([]byte("shard-body-"+string(rune('a'+i))))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finalize(int64(n*64), 2); err != nil {
		t.Fatal(err)
	}
	return *s
}

func TestVerifyClean(t *testing.T) {
	dir := t.TempDir()
	seedStore(t, dir, 4)
	rep, err := Run(dir, params(), Inputs{RoundTripRate: 0.995, KeyframeHitRate: 1.0, BlockSize: 64})
	if err != nil {
		t.Fatal(err)
	}
	if rep.BadSegments != 0 || rep.ResumeReused != 4 {
		t.Fatalf("clean verify: %+v", rep)
	}
	if rep.Segments != 4 || rep.SegmentSHA256Count != 4 {
		t.Fatalf("segment counts: %+v", rep)
	}
	if rep.FirstErrorTick != -1 {
		t.Fatalf("no error expected, got tick %d", rep.FirstErrorTick)
	}
}

func TestVerifyTornTail(t *testing.T) {
	dir := t.TempDir()
	seedStore(t, dir, 4)
	if err := InjectTornTail(dir); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(dir, params(), Inputs{BlockSize: 64})
	if err != nil {
		t.Fatalf("torn tail is a report finding, not a hard error: %v", err)
	}
	if rep.BadSegments != 1 || rep.CorruptRejected != 1 {
		t.Fatalf("torn tail: %+v", rep)
	}
	if rep.ResumeReused != 3 {
		t.Fatalf("intact prefix must still be reused: %+v", rep)
	}
	// Last shard index 3, block size 64 -> first error tick 192.
	if rep.FirstErrorTick != 192 || rep.Corruptions[0].ShardIndex != 3 {
		t.Fatalf("locatable error: %+v", rep.Corruptions)
	}
}

func TestVerifyRejectsChangedParams(t *testing.T) {
	dir := t.TempDir()
	seedStore(t, dir, 2)
	other := params()
	other.DriftPPM = 999
	if _, err := Run(dir, other, Inputs{BlockSize: 64}); !errors.Is(err, checkpoint.ErrParamsChanged) {
		t.Fatalf("want ErrParamsChanged, got %v", err)
	}
}

func TestVerifyMissingState(t *testing.T) {
	_, err := Run(filepath.Join(t.TempDir(), "empty"), params(), Inputs{BlockSize: 64})
	if !errors.Is(err, ErrUnreadable) {
		t.Fatalf("want ErrUnreadable, got %v", err)
	}
}
