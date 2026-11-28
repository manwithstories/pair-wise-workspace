package media

import "testing"

func schedPkt(track string, seq, pts int64, data string) Packet {
	return Packet{
		TrackID: track, Seq: seq, Arrival: seq, PTS: pts, DTS: pts,
		TimeBase: MustRat(1, 30), Data: []byte(data),
	}
}

func TestComparePresentationOrdersByPTSThenTrackThenSeq(t *testing.T) {
	a := schedPkt("a0", 0, 0, "a")
	b := schedPkt("v0", 0, 0, "b")
	// Same PTS: track ID breaks the tie, so a0 precedes v0 regardless of input.
	if ComparePresentation(a, b) >= 0 {
		t.Fatal("a0 should sort before v0 at equal PTS")
	}
	if ComparePresentation(b, a) <= 0 {
		t.Fatal("comparator must be antisymmetric")
	}
	// Same track and PTS: sequence breaks the tie.
	c := schedPkt("v0", 2, 5, "c")
	d := schedPkt("v0", 1, 5, "d")
	if ComparePresentation(d, c) >= 0 {
		t.Fatal("lower sequence should sort first")
	}
}

func TestComparePresentationExactAcrossTimeBases(t *testing.T) {
	// 1 second expressed in two exact bases must compare equal in time even
	// though the raw ticks differ.
	a := Packet{TrackID: "v0", Seq: 0, PTS: 90000, TimeBase: MustRat(1, 90000), Data: []byte("x")}
	b := Packet{TrackID: "v1", Seq: 0, PTS: 1000, TimeBase: MustRat(1, 1000), Data: []byte("y")}
	if ComparePresentation(a, b) != -1 {
		// Equal time, so the track ID decides: v0 before v1.
		t.Fatal("expected track-id tiebreak for equal times")
	}
}

func TestSortPresentationIsDeterministic(t *testing.T) {
	pkts := []Packet{
		schedPkt("v0", 2, 10, "v2"),
		schedPkt("a0", 0, 10, "a0"),
		schedPkt("v0", 1, 5, "v1"),
		schedPkt("a0", 1, 20, "a1"),
	}
	SortPresentation(pkts)
	want := []string{"v1", "a0", "v2", "a1"}
	for i, p := range pkts {
		if string(p.Data) != want[i] {
			t.Fatalf("position %d = %s want %s", i, p.Data, want[i])
		}
	}
	// A second sort of the already sorted slice is a no-op.
	SortPresentation(pkts)
	if string(pkts[0].Data) != "v1" {
		t.Fatal("sort is not idempotent")
	}
}

func TestComputeSegmentHashIgnoresRepresentation(t *testing.T) {
	// The same encoded bytes on the same track hash the same even when the
	// container representation (time base, raw ticks) differs, which is what
	// lets verify recompute a segment from a re-demuxed output.
	a := []Packet{
		{TrackID: "v0", Seq: 0, PTS: 0, TimeBase: MustRat(1, 30), Data: []byte("frame")},
		{TrackID: "v0", Seq: 1, PTS: 1, TimeBase: MustRat(1, 30), Data: []byte("frame2")},
	}
	b := []Packet{
		{TrackID: "v0", Seq: 0, PTS: 0, TimeBase: MustRat(1, 90000), Data: []byte("frame")},
		{TrackID: "v0", Seq: 1, PTS: 3000, TimeBase: MustRat(1, 90000), Data: []byte("frame2")},
	}
	if ComputeSegmentHash(0, a) != ComputeSegmentHash(0, b) {
		t.Fatal("hash must not depend on container representation")
	}
	// Different content must hash differently.
	c := []Packet{
		{TrackID: "v0", Seq: 0, PTS: 0, TimeBase: MustRat(1, 30), Data: []byte("frame")},
		{TrackID: "v0", Seq: 1, PTS: 1, TimeBase: MustRat(1, 30), Data: []byte("other")},
	}
	if ComputeSegmentHash(0, a) == ComputeSegmentHash(0, c) {
		t.Fatal("different content must hash differently")
	}
	// Track association is part of the digest.
	d := []Packet{
		{TrackID: "a1", Seq: 0, PTS: 0, TimeBase: MustRat(1, 30), Data: []byte("frame")},
		{TrackID: "v0", Seq: 1, PTS: 1, TimeBase: MustRat(1, 30), Data: []byte("frame2")},
	}
	if ComputeSegmentHash(0, a) == ComputeSegmentHash(0, d) {
		t.Fatal("track association must affect the hash")
	}
}
