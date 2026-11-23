package merge

import (
	"testing"

	"mediacore/internal/clock"
	"mediacore/internal/sample"
)

func mk(seq int, track uint32, pts clock.Tick) *sample.Sample {
	return &sample.Sample{Seq: seq, TrackID: track, PTS: pts, Data: []byte{byte(seq)}}
}

func TestTotalOrderDeterministic(t *testing.T) {
	// Five shuffled items; sample seq1 (track1, PTS0) is the extra element.
	items := []Ordered{
		{Sample: mk(3, 2, 3000), InputSeq: 3},
		{Sample: mk(1, 1, 0), InputSeq: 1},
		{Sample: mk(2, 1, 3000), InputSeq: 2},
		{Sample: mk(0, 1, 0), InputSeq: 0},
		{Sample: mk(4, 2, 0), InputSeq: 4},
	}
	out := Order(items)
	// PTS0: track1 seq0, track1 seq1, track2 seq4; PTS3000: track1 seq2, track2 seq3.
	wantPTS := []clock.Tick{0, 0, 0, 3000, 3000}
	wantTrack := []uint32{1, 1, 2, 1, 2}
	wantSeq := []int{0, 1, 4, 2, 3}
	if len(out) != 5 {
		t.Fatalf("len %d, want 5", len(out))
	}
	for i := range wantPTS {
		if out[i].PTS != wantPTS[i] || out[i].TrackID != wantTrack[i] ||
			out[i].Seq != wantSeq[i] {
			t.Fatalf("pos %d got pts=%d track=%d seq=%d", i,
				out[i].PTS, out[i].TrackID, out[i].Seq)
		}
	}
}

func TestOrderConcurrencyIndependent(t *testing.T) {
	base := []Ordered{
		{Sample: mk(0, 1, 0), InputSeq: 0},
		{Sample: mk(1, 2, 1000), InputSeq: 1},
		{Sample: mk(2, 1, 2000), InputSeq: 2},
		{Sample: mk(3, 3, 2000), InputSeq: 3},
		{Sample: mk(4, 2, 3000), InputSeq: 4},
	}
	// Two different arrival permutations must collapse to the same order.
	permA := append([]Ordered{}, base...)
	permB := []Ordered{base[4], base[2], base[0], base[3], base[1]}
	if !Equal(Order(permA), Order(permB)) {
		t.Fatal("different arrival order produced different merged sequence")
	}
}

func TestReorderBufferFlush(t *testing.T) {
	rb := &ReorderBuffer{}
	rb.Add(2, mk(2, 1, 6000))
	rb.Add(0, mk(0, 1, 0))
	rb.Add(1, mk(1, 1, 3000))
	if rb.Len() != 3 {
		t.Fatalf("len %d", rb.Len())
	}
	out := rb.Flush()
	if out[0].PTS != 0 || out[1].PTS != 3000 || out[2].PTS != 6000 {
		t.Fatal("flush not ordered")
	}
	if rb.Len() != 0 {
		t.Fatal("flush must reset buffer")
	}
}

func TestEqualPayloadSensitive(t *testing.T) {
	a := []*sample.Sample{mk(0, 1, 0)}
	b := []*sample.Sample{mk(0, 1, 0)}
	b[0].Data = []byte{99}
	if Equal(a, b) {
		t.Fatal("payload difference must make Equal false")
	}
}
