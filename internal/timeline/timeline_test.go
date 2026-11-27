package timeline

import (
	"encoding/binary"
	"testing"

	"archiver/internal/media"
)

var (
	trA = media.TrackID{Kind: media.TrackAudio, SSRC: 0xaa}
	trV = media.TrackID{Kind: media.TrackVideo, SSRC: 0xbb}
)

// testSink records emitted events; snapshots capture the emitted count.
type testSink struct{ emitted []Event }

func (s *testSink) Emit(ev Event) { s.emitted = append(s.emitted, ev) }
func (s *testSink) Snapshot() []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(len(s.emitted)))
	return b[:]
}
func (s *testSink) Restore(snap []byte) {
	s.emitted = s.emitted[:binary.BigEndian.Uint64(snap)]
}

func ev(track media.TrackID, us media.TimeUs, seq uint64) Event {
	return Event{Track: track, TimeUs: us, ExtSeq: seq}
}

func times(s *testSink) []media.TimeUs {
	var out []media.TimeUs
	for _, e := range s.emitted {
		out = append(out, e.TimeUs)
	}
	return out
}

func newHarness(lateness, horizon media.TimeUs) (*Replayer, *testSink) {
	wm := NewWatermark(lateness)
	sink := &testSink{}
	r := NewReplayer(wm, horizon, 4, sink, func() []byte { return nil }, func([]byte) {})
	return r, sink
}

func TestWatermarkAdvancesWithSlowestTrack(t *testing.T) {
	r, sink := newHarness(1_000_000, 5_000_000)
	r.Add(ev(trA, 3_000_000, 1))
	if len(sink.emitted) != 0 {
		t.Fatal("video track silent: nothing may commit")
	}
	r.Add(ev(trV, 2_500_000, 1))
	// watermark = min(3.0, 2.5) - 1.0 = 1.5s: still nothing <= 1.5s.
	if len(sink.emitted) != 0 {
		t.Fatalf("emitted %v", times(sink))
	}
	r.Add(ev(trV, 4_500_000, 2))
	// watermark = min(3.0, 4.5) - 1.0 = 2.0s: video@2.5 not yet.
	r.Add(ev(trA, 6_000_000, 2))
	// watermark = min(6.0, 4.5) - 1.0 = 3.5s: commits <= 3.5s.
	want := []media.TimeUs{2_500_000, 3_000_000}
	got := times(sink)
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestLateWithinHorizonReplays(t *testing.T) {
	r, sink := newHarness(1_000_000, 5_000_000)
	r.Add(ev(trA, 1_000_000, 1))
	r.Add(ev(trV, 1_000_000, 1))
	r.Add(ev(trA, 5_000_000, 2))
	r.Add(ev(trV, 5_000_000, 2)) // watermark = 4s: commits the 1s pair
	if len(sink.emitted) != 2 {
		t.Fatalf("emitted %v", times(sink))
	}
	// Late video event at 2s: inside the 5s horizon -> rollback+replay.
	r.Add(ev(trV, 2_000_000, 3))
	if r.Replays != 1 {
		t.Fatalf("replays = %d, want 1", r.Replays)
	}
	want := []media.TimeUs{1_000_000, 1_000_000, 2_000_000}
	got := times(sink)
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestLateBeyondHorizonQuarantined(t *testing.T) {
	r, sink := newHarness(1_000_000, 2_000_000)
	r.Add(ev(trA, 1_000_000, 1))
	r.Add(ev(trV, 1_000_000, 1))
	r.Add(ev(trA, 10_000_000, 2))
	r.Add(ev(trV, 10_000_000, 2)) // watermark = 9s
	before := len(sink.emitted)
	r.Add(ev(trV, 5_000_000, 3)) // 4s older than drained 9s > 2s horizon
	if r.Quarantined != 1 {
		t.Fatalf("quarantined = %d, want 1", r.Quarantined)
	}
	if len(sink.emitted) != before {
		t.Fatal("quarantined event must not be emitted")
	}
}

func TestStableOrderSameTimestamp(t *testing.T) {
	r, sink := newHarness(0, 5_000_000)
	r.Add(ev(trV, 1_000_000, 7))
	r.Add(ev(trA, 1_000_000, 3))
	r.Add(ev(trA, 2_000_000, 4))
	r.Add(ev(trV, 2_000_000, 8))
	r.Flush()
	got := sink.emitted
	// Same timestamp: audio SSRC 0xaa < video SSRC 0xbb sorts first.
	if got[0].Track.SSRC != 0xaa || got[1].Track.SSRC != 0xbb {
		t.Fatalf("unstable order: %+v", got)
	}
}
