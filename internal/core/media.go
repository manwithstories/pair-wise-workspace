package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
)

// TrackKind identifies the essence family supported by FieldPost.
type TrackKind string

const (
	TrackVideo TrackKind = "avc"
	TrackAudio TrackKind = "aac"
)

// Track describes one AVC or AAC track and its integer timebase.
type Track struct {
	ID        uint32
	Kind      TrackKind
	Codec     string
	Timescale uint32
	Width     uint16
	Height    uint16
	Channels  uint16
	Sequence  int
}

// Sample flag bits are deliberately stable across TS and fMP4.
const (
	FlagSync    uint32 = 1 << 0
	FlagDepends uint32 = 1 << 1
	FlagNonSync uint32 = 1 << 2
)

// Sample is one encoded media access unit. Data is never decoded.
type Sample struct {
	TrackID    uint32
	PTS        Ticks
	DTS        Ticks
	Duration   Ticks
	Flags      uint32
	Data       []byte
	DataOffset int64
}

// Segment is a recovered media shard and its ordered essence.
type Segment struct {
	ID        string
	Camera    string
	Index     int
	Source    string
	Manifest  string
	Tracks    []Track
	Samples   []Sample
	Recovered bool
	Retries   int
}

// TrackByID returns the track with id.
func (s *Segment) TrackByID(id uint32) (Track, bool) {
	for _, t := range s.Tracks {
		if t.ID == id {
			return t, true
		}
	}
	return Track{}, false
}

// SortedSamples returns samples in deterministic track/decode/presentation order
// without mutating the segment's wire order.
func (s *Segment) SortedSamples() []Sample {
	out := append([]Sample(nil), s.Samples...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TrackID != out[j].TrackID {
			return out[i].TrackID < out[j].TrackID
		}
		if out[i].DTS != out[j].DTS {
			return out[i].DTS < out[j].DTS
		}
		return out[i].PTS < out[j].PTS
	})
	return out
}

// WriteSampleHash writes the canonical sample commitment. It includes ordered
// essence bytes, integer timestamps, duration and flags, but not parser-only
// offsets or transport retry counts.
func WriteSampleHash(h hash.Hash, sm Sample) {
	var u64 [8]byte
	put := func(v int64) {
		binary.BigEndian.PutUint64(u64[:], uint64(v))
		_, _ = h.Write(u64[:])
	}
	put(int64(sm.TrackID))
	put(int64(sm.PTS))
	put(int64(sm.DTS))
	put(int64(sm.Duration))
	put(int64(sm.Flags))
	put(int64(len(sm.Data)))
	_, _ = h.Write(sm.Data)
}

// EssenceHash returns the stable hex SHA-256 commitment for one segment.
func EssenceHash(seg *Segment) string {
	h := sha256.New()
	samples := seg.SortedSamples()
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(samples)))
	_, _ = h.Write(n[:])
	for _, sm := range samples {
		WriteSampleHash(h, sm)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// VerifyEssence compares two ordered sample sets using the canonical hash and
// returns a structured invariant error on the first mismatch.
func VerifyEssence(stage Stage, source string, want, got []Sample) error {
	want = (&Segment{Samples: want}).SortedSamples()
	got = (&Segment{Samples: got}).SortedSamples()
	hw := sha256.New()
	hg := sha256.New()
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(want)))
	_, _ = hw.Write(n[:])
	binary.BigEndian.PutUint32(n[:], uint32(len(got)))
	_, _ = hg.Write(n[:])
	for _, sm := range want {
		WriteSampleHash(hw, sm)
	}
	for _, sm := range got {
		WriteSampleHash(hg, sm)
	}
	a := hw.Sum(nil)
	b := hg.Sum(nil)
	if !bytes.Equal(a, b) {
		off := int64(-1)
		for i := 0; i < len(want) && i < len(got); i++ {
			if want[i].PTS != got[i].PTS || want[i].DTS != got[i].DTS ||
				want[i].Flags != got[i].Flags || !bytes.Equal(want[i].Data, got[i].Data) {
				off = int64(i)
				break
			}
		}
		return InvariantError(stage, source, off,
			"essence hash mismatch want=%x got=%x", a[:6], b[:6])
	}
	return nil
}
