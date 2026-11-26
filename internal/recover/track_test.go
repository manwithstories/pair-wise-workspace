package recover

import (
	"math/rand"
	"testing"

	"mediad/internal/index"
	"mediad/internal/media"
)

func testTrack() *media.Track {
	tr := &media.Track{ID: 7, Timescale: 90_000}
	for i := uint32(0); i < 40; i++ {
		tr.Samples = append(tr.Samples, media.Sample{
			Index: i + 1, PTS: int64(i) * 3000, DTS: int64(i) * 3000, Duration: 3000,
			Offset: int64(100 + i*32), Size: 32, Sync: i%10 == 0,
		})
	}
	return tr
}

func verifiedIndex(tr *media.Track) *index.Index {
	idx, _ := index.Rebuild(&media.Project{Tracks: []*media.Track{tr}})
	for _, ti := range idx.Tracks {
		for i := range ti.Points {
			ti.Points[i].Verified = true
		}
	}
	idx.Complete = true
	return idx
}

func TestRunTrackRecoversBurstFromRetransmitOrSync(t *testing.T) {
	tr := testTrack()
	idx := verifiedIndex(tr)
	r1 := RunTrack(tr, idx, rand.New(rand.NewSource(42)), DefaultConfig(0.3, 7))
	r2 := RunTrack(tr, idx, rand.New(rand.NewSource(42)), DefaultConfig(0.3, 7))
	if r1.LostPackets == 0 || r1.Retransmissions == 0 {
		t.Fatalf("expected loss and retransmissions: %#v", r1)
	}
	if r1.RecoveryPct < 99 {
		t.Fatalf("recovery=%.2f report=%#v", r1.RecoveryPct, r1)
	}
	if r1.UnrecoveredPackets != 0 {
		t.Fatalf("unrecovered=%d faults=%#v", r1.UnrecoveredPackets, r1.Faults)
	}
	// Fixed seed must reproduce the exact audit.
	if r1.LostPackets != r2.LostPackets || r1.Retransmissions != r2.Retransmissions || r1.RecoveryPct != r2.RecoveryPct {
		t.Fatalf("nondeterministic recovery: %#v vs %#v", r1, r2)
	}
}

func TestCorruptOffsetWithoutVerifiedCheckpointIsLocatableFailure(t *testing.T) {
	tr := testTrack()
	for i := range tr.Samples {
		tr.Samples[i].Offset = -1
	}
	rep := RunTrack(tr, &index.Index{Tracks: map[uint32]*index.TrackIndex{}}, rand.New(rand.NewSource(1)), DefaultConfig(0.9, 4))
	if rep.UnrecoveredPackets == 0 || len(rep.Faults) == 0 {
		t.Fatalf("expected locatable recovery failures: %#v", rep)
	}
	f := rep.Faults[0]
	if f.TrackID != 7 || f.TimeSpan == nil {
		t.Fatalf("fault lacks track/time span: %#v", f)
	}
	if f.Recoverable {
		t.Fatal("recovery fault should be terminal for the affected track")
	}
}

func TestPositiveOutOfBoundsOffsetBlocksRecovery(t *testing.T) {
	tr := testTrack()
	tr.Samples[3].Offset = 1 << 30
	cfg := DefaultConfig(1, 1)
	cfg.FileSize = 5000
	rep := RunTrack(tr, &index.Index{Tracks: map[uint32]*index.TrackIndex{}}, rand.New(rand.NewSource(2)), cfg)
	if rep.UnrecoveredPackets == 0 {
		t.Fatal("expected out-of-bounds offset corruption to affect recovery")
	}
	var found bool
	for _, f := range rep.Faults {
		if f.ByteRange != nil && f.ByteRange.Offset == 1<<30 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing locatable offset fault: %#v", rep.Faults)
	}
}

func TestEnginesAreIndependent(t *testing.T) {
	tr := testTrack()
	idx := verifiedIndex(tr)
	e7 := NewEngine(7, idx)
	e8 := NewEngine(8, idx)
	if _, ok := e7.NearestCheckpoint(15); !ok {
		t.Fatal("track 7 missing checkpoint")
	}
	if _, ok := e8.NearestCheckpoint(15); ok {
		t.Fatal("track 8 unexpectedly saw track 7 checkpoint")
	}
}
