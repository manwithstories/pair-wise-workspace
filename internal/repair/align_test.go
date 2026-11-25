package repair_test

import (
	"testing"

	"github.com/pairwise/mediad/internal/fixture"
	"github.com/pairwise/mediad/internal/ingest"
	"github.com/pairwise/mediad/internal/media"
	"github.com/pairwise/mediad/internal/repair"
)

func TestCleanFixtureAlignsAllTwelveAndCountsEvidence(t *testing.T) {
	b := fixture.Generate(73, fixture.ScenarioClean)
	r, err := repair.Align(b.Tracks, b.Steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Segments) != 12 {
		t.Fatalf("segments=%d", len(r.Segments))
	}
	if r.Duplicates != 24 {
		t.Fatalf("duplicates=%d want24", r.Duplicates)
	}
	if r.BurstLosses != 4 {
		t.Fatalf("burst=%d want4", r.BurstLosses)
	}
	if r.BandwidthSteps != 5 || len(r.WindowTrace) != 12 {
		t.Fatalf("steps=%d trace=%d", r.BandwidthSteps, len(r.WindowTrace))
	}
	for _, s := range r.Segments {
		if !s.Complete || !s.Keyframe || len(s.Data) == 0 {
			t.Fatalf("segment %d incomplete: %+v", s.GlobalIndex, s)
		}
	}
	if len(r.TimelineEvents) == 0 {
		t.Fatal("expected tfdt fallback evidence")
	}
}

func TestUnrecoveredGapMarksIncompleteAndNoKeyframe(t *testing.T) {
	track := repair.TrackInput{ID: "t", Number: 9, Timescale: 90000}
	seg := repair.SegmentInput{
		GlobalIndex: 0, SegmentInTrack: 0, TrackID: "t", TrackNumber: 9,
		Timescale: 90000, DurationUnits: 180000, TFDT: 0, Keyframe: true,
		SeqStart: 100, SeqEnd: 102, BandwidthBps: 500000,
		Payload: []byte("partial"),
		Arrival: []ingest.Packet{
			{Sequence: 100, Payload: []byte("a")},
			{Sequence: 102, Payload: []byte("c")},
		},
	}
	track.Segments = []repair.SegmentInput{seg}
	r, err := repair.Align([]repair.TrackInput{track}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.BurstLosses != 1 || r.Incomplete != 1 {
		t.Fatalf("burst=%d incomplete=%d", r.BurstLosses, r.Incomplete)
	}
	if r.Segments[0].Keyframe || r.Segments[0].Complete || len(r.Segments[0].Data) != 0 {
		t.Fatal("incomplete segment must not contain fabricated keyframe fragment")
	}
	gap := ingest.Gap{From: 101, To: 101, Count: 1}
	if !repair.WindowFull(gap, 120, 16) {
		t.Fatal("gap should be expired after a full window")
	}
}

func TestFullBoundedWindowExpiresGap(t *testing.T) {
	var arrival []ingest.Packet
	for seq := uint64(200); seq <= 220; seq++ {
		if seq == 205 {
			continue
		}
		arrival = append(arrival, ingest.Packet{Sequence: uint16(seq), Payload: []byte("p")})
	}
	track := repair.TrackInput{ID: "window", Number: 7, Timescale: 90000}
	track.Segments = []repair.SegmentInput{{
		GlobalIndex: 0, TrackID: "window", Timescale: 90000,
		DurationUnits: 180000, Keyframe: true, SeqStart: 200, SeqEnd: 220,
		BandwidthBps: 500_000, Arrival: arrival, Payload: []byte("x"),
	}}
	r, err := repair.Align([]repair.TrackInput{track}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Segments[0]
	if s.Complete || s.Keyframe || !s.WindowExpired || s.Window != 16 {
		t.Fatalf("segment=%+v", s)
	}
}

func TestConflictSampleReturnsStructuredError(t *testing.T) {
	s := ingest.NewStream(8)
	_ = s.Accept(fixture.Generate(1, fixture.ScenarioDamaged).ConflictA)
	err := s.Accept(fixture.Generate(1, fixture.ScenarioDamaged).ConflictB)
	if media.ExitCode(err) != 2 {
		t.Fatalf("err=%v", err)
	}
}

func TestTruncatedAndOversizeBoxesRejected(t *testing.T) {
	b := fixture.Generate(73, fixture.ScenarioDamaged)
	if err := repair.ValidateBoxes(b.TruncatedBox); media.ExitCode(err) != 2 {
		t.Fatalf("truncated err=%v", err)
	}
	err := repair.ValidateBoxes(b.OversizeBox)
	if me, ok := media.AsError(err); !ok || me.Code != media.CodeTooLarge {
		t.Fatalf("oversize err=%v", err)
	}
}
