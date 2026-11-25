package fingerprint

import (
	"testing"

	"mediaproc/internal/demux"
	"mediaproc/internal/model"
)

func TestNormalizationIgnoresTimestampAndTrack(t *testing.T) {
	mk := func(track uint64, ts int64) *demux.Block {
		return &demux.Block{
			TrackNumber: track,
			TimeNS:      model.Nanoseconds(ts),
			Track:       &demux.Track{CodecID: "A_OPUS", Type: 2},
			Frames:      [][]byte{{1, 2, 3, 4}},
		}
	}
	a := Sum(mk(2, 100))
	b := Sum(mk(9, 999999))
	if a.Hash != b.Hash {
		t.Fatalf("normalized hashes differ: %s vs %s", a.Hash, b.Hash)
	}
}

func TestEqualHashStillComparesPayload(t *testing.T) {
	s := NewStore()
	if s.Check(Fingerprint{Hash: "collision-hash", Payload: []byte("alpha")}) {
		t.Fatal("first fingerprint must not be duplicate")
	}
	if s.Check(Fingerprint{Hash: "collision-hash", Payload: []byte("beta")}) {
		t.Fatal("hash collision with different payload must not be deduplicated")
	}
	if !s.Check(Fingerprint{Hash: "collision-hash", Payload: []byte("alpha")}) {
		t.Fatal("identical hash and payload should be duplicate")
	}
	if s.Collisions != 1 || s.Duplicates != 1 {
		t.Fatalf("collisions=%d duplicates=%d", s.Collisions, s.Duplicates)
	}
}
