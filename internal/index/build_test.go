package index

import (
	"bytes"
	"testing"

	"archivecheck/internal/clock"
	"archivecheck/internal/fault"
	"archivecheck/internal/timeline"
)

// fakeSource implements Verifier over a map of known block offsets.
type fakeSource struct {
	blocks map[int64]BlockRef
}

func (f fakeSource) VerifyBlock(off int64) (BlockRef, error) {
	b, ok := f.blocks[off]
	if !ok {
		return BlockRef{}, errNotBlock
	}
	return b, nil
}

type blockErr struct{}

func (blockErr) Error() string { return "not a block boundary" }

var errNotBlock = blockErr{}

func buildTimeline(t *testing.T) (*timeline.TrackInfo, []int64) {
	t.Helper()
	track := &timeline.TrackInfo{Number: 1, Kind: "video"}
	offsets := []int64{}
	for i := 0; i < 6; i++ {
		track.Frames = append(track.Frames, timeline.Frame{
			Track: 1, Index: i, Original: clock.MustNew(int64(i*40), 1000),
			Repaired: clock.MustNew(int64(i*40), 1000), Offset: int64(100 + i*20),
			Keyframe: i%3 == 0, Bytes: 20, Digest: uint32(i),
		})
		offsets = append(offsets, int64(100+i*20))
	}
	return track, offsets
}

func sourceFor(track *timeline.TrackInfo, skip int64) fakeSource {
	m := map[int64]BlockRef{}
	for _, f := range track.Frames {
		if f.Offset == skip {
			continue
		}
		m[f.Offset] = BlockRef{Track: 1, Offset: f.Offset, PayloadOff: f.Offset + 4, PayloadLen: 16, Element: "SimpleBlock", Keyframe: f.Keyframe, Digest: f.Digest, Time: f.Repaired}
	}
	return fakeSource{blocks: m}
}

func TestKeyframeIndexOnlyAtRealOffsets(t *testing.T) {
	track, _ := buildTimeline(t)
	src := sourceFor(track, -1)
	fs := &fault.Set{}
	idx, err := Build(map[uint64]*timeline.TrackInfo{1: track}, []uint64{1}, BuildConfig{KeyframesOnly: true}, src, fs)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 2 {
		t.Fatalf("entries=%d want 2", len(idx.Entries))
	}
	for _, e := range idx.Entries {
		if _, err := src.VerifyBlock(e.Offset); err != nil {
			t.Fatalf("entry offset %d not a block", e.Offset)
		}
		if !e.Keyframe {
			t.Fatal("non-keyframe entry in keyframe-only index")
		}
		if e.PayloadOff != e.Offset+4 || e.PayloadLen != 16 {
			t.Fatalf("entry geometry: %+v", e)
		}
	}
	if fs.Len() != 0 {
		t.Fatalf("faults: %v", fs.Faults())
	}
}

func TestMisalignedCandidateRejected(t *testing.T) {
	track, offsets := buildTimeline(t)
	src := sourceFor(track, offsets[3]) // remove the third keyframe boundary
	fs := &fault.Set{}
	idx, err := Build(map[uint64]*timeline.TrackInfo{1: track}, []uint64{1}, BuildConfig{KeyframesOnly: true}, src, fs)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 1 {
		t.Fatalf("entries=%d want 1", len(idx.Entries))
	}
	if fs.Count(fault.KindIndexMisaligned) != 1 {
		t.Fatalf("faults: %v", fs.Faults())
	}
}

func TestRepeatedBuildSameDigest(t *testing.T) {
	track, _ := buildTimeline(t)
	src := sourceFor(track, -1)
	idx1, _ := Build(map[uint64]*timeline.TrackInfo{1: track}, []uint64{1}, BuildConfig{}, src, &fault.Set{})
	idx2, _ := Build(map[uint64]*timeline.TrackInfo{1: track}, []uint64{1}, BuildConfig{}, src, &fault.Set{})
	if idx1.Digest != idx2.Digest {
		t.Fatalf("digest differs: %s vs %s", idx1.Digest, idx2.Digest)
	}
	if len(idx1.Entries) != 6 {
		t.Fatalf("entries=%d", len(idx1.Entries))
	}
}

func TestSampleEveryAndCap(t *testing.T) {
	track, _ := buildTimeline(t)
	src := sourceFor(track, -1)
	idx, _ := Build(map[uint64]*timeline.TrackInfo{1: track}, []uint64{1}, BuildConfig{SampleEvery: 2, MaxEntriesPerTrack: 2}, src, &fault.Set{})
	if len(idx.Entries) != 2 {
		t.Fatalf("entries=%d", len(idx.Entries))
	}
}

func TestSeekReturnsContainingGOP(t *testing.T) {
	track, _ := buildTimeline(t)
	src := sourceFor(track, -1)
	idx, _ := Build(map[uint64]*timeline.TrackInfo{1: track}, []uint64{1}, BuildConfig{KeyframesOnly: true}, src, &fault.Set{})
	// Keyframes are at 0ms and 120ms.  Seeking 100ms lands on the 0ms entry.
	e, ok := idx.Seek(1, clock.MustNew(100, 1000))
	if !ok || e.TimeUsec != 0 {
		t.Fatalf("seek got %+v ok=%v", e, ok)
	}
	e, ok = idx.Seek(1, clock.MustNew(150, 1000))
	if !ok || e.TimeUsec != 120_000 {
		t.Fatalf("seek got %+v", e)
	}
	if _, ok := idx.Seek(2, clock.MustNew(0, 1000)); ok {
		t.Fatal("seek on absent track should fail")
	}
}

func TestSidecarRoundTripWithGoMP4(t *testing.T) {
	track, _ := buildTimeline(t)
	src := sourceFor(track, -1)
	idx, _ := Build(map[uint64]*timeline.TrackInfo{1: track}, []uint64{1}, BuildConfig{}, src, &fault.Set{})
	var buf bytes.Buffer
	sc := NewSidecar(idx)
	if err := sc.Write(&buf); err != nil {
		t.Fatal(err)
	}
	got, note, err := ParseSidecar(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.MajorBrand != sc.MajorBrand {
		t.Fatalf("brand=%q", got.MajorBrand)
	}
	if got.MinorVersion != 1 {
		t.Fatalf("minor=%d", got.MinorVersion)
	}
	if len(got.CompatibleBrands) != 2 {
		t.Fatalf("brands=%v", got.CompatibleBrands)
	}
	if !bytes.Contains([]byte(note), []byte(idx.Digest)) {
		t.Fatalf("note=%q missing digest", note)
	}
}

func TestIndexJSONDeterministic(t *testing.T) {
	track, _ := buildTimeline(t)
	src := sourceFor(track, -1)
	mk := func() []byte {
		idx, _ := Build(map[uint64]*timeline.TrackInfo{1: track}, []uint64{1}, BuildConfig{}, src, &fault.Set{})
		var b bytes.Buffer
		if err := idx.WriteJSON(&b); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	if !bytes.Equal(mk(), mk()) {
		t.Fatal("index JSON not deterministic")
	}
}
