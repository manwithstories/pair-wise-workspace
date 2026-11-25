package archivecheck_test

import (
	"testing"

	"mediaarchive/internal/archive"
	"mediaarchive/internal/clock"
	"mediaarchive/internal/ingest"
	"mediaarchive/internal/timeline"
)

func testPacket(no int, track string, seq int64, start, dur clock.Ticks, hash string) ingest.Packet {
	return ingest.Packet{
		No: no, TrackID: track, Codec: "opus", Seq: uint16(seq), ExtSeq: seq,
		Start: start, Duration: dur, Hash: hash,
		Source: ingest.SourceRef{Path: "fixtures/src.rtprec", Offset: int64(no) * 64, Length: 24},
	}
}

func TestDedupIdempotentAndContentConflictQuarantined(t *testing.T) {
	s := timeline.NewState()
	first := testPacket(1, "track", 100, 0, 960, "aaa")
	dup := testPacket(2, "track", 100, 0, 960, "aaa")
	conflict := testPacket(3, "track", 100, 0, 960, "bbb")

	if r := s.Apply(first); !r.Accepted {
		t.Fatal("first packet should be accepted")
	}
	if r := s.Apply(dup); !r.Duplicate {
		t.Fatal("identical retry should be duplicate")
	}
	if r := s.Apply(conflict); !r.Conflict {
		t.Fatal("different hash should be conflict")
	}
	if s.AcceptedCount != 1 || s.DuplicateCount != 1 || s.ConflictCount != 1 {
		t.Fatalf("counts accepted=%d dup=%d conflict=%d", s.AcceptedCount, s.DuplicateCount, s.ConflictCount)
	}
	if len(s.Conflicts) != 1 || len(s.Conflicts[0].Locations) != 2 {
		t.Fatalf("conflict locations missing: %+v", s.Conflicts)
	}
	for _, p := range s.CanonicalPackets() {
		if p.Identity() == first.Identity() {
			t.Fatal("conflicted identity must be quarantined from canonical timeline")
		}
	}
}

func TestSequenceHoleUsesEventTimeNotArrivalOrder(t *testing.T) {
	s := timeline.NewState()
	s.Apply(testPacket(1, "t", 1, 0, 960, "h1"))
	s.Apply(testPacket(2, "t", 3, 1920, 960, "h3"))
	if len(s.Holes) != 1 {
		t.Fatalf("holes=%d want 1", len(s.Holes))
	}
	h := s.Holes[0]
	if h.FirstMissing != 2 || h.LastMissing != 2 {
		t.Fatalf("missing range=%d..%d", h.FirstMissing, h.LastMissing)
	}
	if h.StartTicks != 960 || h.EndTicks != 1920 {
		t.Fatalf("hole interval=[%d,%d), want [960,1920)", h.StartTicks, h.EndTicks)
	}
	if h.StartSource == 0 || h.EndSource == 0 {
		t.Fatalf("hole boundaries lack source offsets: %+v", h)
	}
}

func TestOutOfOrderPacketsMergeByEventTime(t *testing.T) {
	s := timeline.NewState()
	s.Apply(testPacket(1, "t", 2, 1000, 1000, "later"))
	s.Apply(testPacket(2, "t", 1, 0, 1000, "earlier"))
	got := s.CanonicalPackets()
	if len(got) != 2 || got[0].Hash != "earlier" || got[1].Hash != "later" {
		t.Fatalf("order wrong: %+v", got)
	}
}

func TestLateEventReplayMatchesCheckpointRecovery(t *testing.T) {
	events := []ingest.Packet{
		testPacket(1, "t", 1, 0, 1000, "h1"),
		testPacket(2, "t", 2, 1000, 1000, "h2"),
		testPacket(3, "t", 3, 2000, 1000, "h3"),
		testPacket(4, "t", 4, 3000, 1000, "h4"),
		testPacket(5, "t", 5, 200000, 1000, "h5"),
		testPacket(6, "t", 1, 0, 1000, "h1"), // 跨水位的旧包重传
	}

	full := timeline.NewReplayer(timeline.NewState(), events, 1000)
	full.Interval = 1
	if err := full.ProcessAll(0); err != nil {
		t.Fatal(err)
	}
	if full.State.ReplayCount != 1 || len(full.State.LateEvents) != 1 {
		t.Fatalf("replays=%d late=%d", full.State.ReplayCount, len(full.State.LateEvents))
	}
	if full.State.LateEvents[0].CheckpointCursor != 5 {
		t.Fatalf("nearest checkpoint=%d want 5", full.State.LateEvents[0].CheckpointCursor)
	}

	base := timeline.Reduce(timeline.NewState(), events[:3])
	recovered := timeline.NewReplayer(base, events, 1000)
	recovered.Interval = 1
	if err := recovered.ProcessAll(0); err != nil {
		t.Fatal(err)
	}

	h1, err := archive.StateHash(full.State)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := archive.StateHash(recovered.State)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("recovered state hash %s != full replay %s", h2, h1)
	}
	if len(recovered.State.CanonicalPackets()) != 5 {
		t.Fatalf("canonical packets=%d want 5 (late retry is duplicate)", len(recovered.State.CanonicalPackets()))
	}
}
