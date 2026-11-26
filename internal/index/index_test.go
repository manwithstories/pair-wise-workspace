package index_test

import (
	"testing"

	"avpipe/internal/fixture"
	"avpipe/internal/index"
	"avpipe/internal/ts"
)

func buildFixtureIndex(t *testing.T, c fixture.Case, ri int) (*index.Index, []ts.Packet) {
	t.Helper()
	srcs, err := fixture.Generate(fixture.Config{Seed: 23, Case: c, Bitrates: []int64{600000, 1200000}, Segments: 12})
	if err != nil {
		t.Fatal(err)
	}
	pkts, _, err := ts.ParseStream(srcs[ri].Data)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := index.Build(pkts, index.BuildConfig{
		Rendition: srcs[ri].Rendition, Bitrate: srcs[ri].Bitrate,
		VideoPID: fixture.VideoPID, AudioPID: fixture.AudioPID,
		ExpectedSlots: 12, PacketsPerSlot: fixture.PacketsPerSlot,
	})
	if err != nil {
		t.Fatal(err)
	}
	return idx, pkts
}

func TestIndexFindsAllIDRAndAudioHits(t *testing.T) {
	idx, _ := buildFixtureIndex(t, fixture.CaseClean, 0)
	if len(idx.Keyframes) != 12 {
		t.Fatalf("keyframes=%d want 12", len(idx.Keyframes))
	}
	if len(idx.Audio) != 12 {
		t.Fatalf("audio stamps=%d want 12", len(idx.Audio))
	}
	if missing := idx.MarkMissingSlots(); len(missing) != 0 {
		t.Fatalf("unexpected missing slots: %v", missing)
	}
	for i, k := range idx.Keyframes {
		if k.Seq != i {
			t.Fatalf("keyframe %d mapped seq=%d", i, k.Seq)
		}
		if k.Tick == 0 {
			t.Fatalf("keyframe %d has no expanded PTS", i)
		}
	}
}

func TestIndexMissingIDRAndCCGapAffectSlotOnce(t *testing.T) {
	idx, _ := buildFixtureIndex(t, fixture.CaseDamaged, 0)
	if len(idx.Keyframes) != 11 {
		t.Fatalf("keyframes=%d want 11", len(idx.Keyframes))
	}
	missing := idx.MarkMissingSlots()
	if len(missing) != 1 || missing[0] != 11 {
		t.Fatalf("missing=%v want [11]", missing)
	}
	if len(idx.CCGaps) != 1 {
		t.Fatalf("cc gaps=%d want 1", len(idx.CCGaps))
	}
	var ccAffected, missingAffected int
	for _, a := range idx.AffectedIntervals {
		if a.Seq != 11 {
			t.Fatalf("affected seq=%d want 11", a.Seq)
		}
		switch a.Reason {
		case "continuity_counter_gap":
			ccAffected++
		case "missing_idr":
			missingAffected++
		}
	}
	if ccAffected != 1 || missingAffected != 1 {
		t.Fatalf("affected counts cc=%d missing=%d", ccAffected, missingAffected)
	}
}

func TestNALIDRDetection(t *testing.T) {
	idr := []byte{0, 0, 0, 1, 0x65, 0x88}
	nonIDR := []byte{0, 0, 1, 0x61, 0x00}
	if !index.NALContainsIDR(idr) {
		t.Fatal("long start-code IDR not detected")
	}
	if index.NALContainsIDR(nonIDR) {
		t.Fatal("non-IDR slice falsely detected")
	}
}
