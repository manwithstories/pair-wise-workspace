package index

import (
	"bytes"
	"errors"
	"testing"

	"archivecheck.local/archivecheck/internal/demux"
	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/fixture"
)

func sample(track uint64, tick int64, key bool, offset int64) demux.Sample {
	return demux.Sample{TrackNumber: track, Timestamp: tick, Keyframe: key, SourceOffset: offset, ClusterOffset: offset}
}

func TestAtOrBeforeAndHit(t *testing.T) {
	ix := FromSamples([]demux.Sample{
		sample(1, 0, true, 100),
		sample(1, 40, false, 200),
		sample(1, 80, true, 300),
		sample(1, 120, false, 400),
		sample(1, 160, true, 500),
		sample(2, 0, true, 90),
	})
	if e, ok := ix.AtOrBefore(1, 100); !ok || e.SourceOffset != 300 {
		t.Fatalf("got %+v %v", e, ok)
	}
	if !ix.Hit(1, 100, 300) {
		t.Fatal("expected hit")
	}
	if ix.Hit(1, 100, 100) {
		t.Fatal("wrong offset must not hit")
	}
	if _, ok := ix.AtOrBefore(1, -1); ok {
		t.Fatal("no keyframe before zero")
	}
	if !ix.Rebuilt {
		t.Fatal("sample-derived index must be marked rebuilt")
	}
}

func TestFromSamplesSkipsNonKeyframes(t *testing.T) {
	ix := FromSamples([]demux.Sample{sample(1, 0, false, 10), sample(1, 10, true, 20)})
	if len(ix.Entries) != 1 || ix.Entries[0].SourceOffset != 20 {
		t.Fatalf("entries=%+v", ix.Entries)
	}
}

func TestParseCuesRoundTrip(t *testing.T) {
	doc := fixture.Build(fixture.DefaultTracks(), fixture.DefaultBlocks(), fixture.Layout{SortByTime: true, ClusterSpan: 9})
	payload := cuesPayload(t, doc.Bytes)
	points, err := ParseCues(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) == 0 {
		t.Fatal("no cue points parsed")
	}
	if points[0].Track == 0 || points[0].ClusterPosition < 0 {
		t.Fatalf("bad cue point: %+v", points[0])
	}
}

func cuesPayload(t *testing.T, data []byte) []byte {
	t.Helper()
	// Locate the Cues element by its ID and read its declared size.
	id := fixture.EncID(0x1C53BB6B)
	i := bytes.Index(data, id)
	if i < 0 {
		t.Fatal("no Cues element")
	}
	r := bytes.NewReader(data[i+len(id):])
	// Decode the size vint manually.
	var head [8]byte
	if _, err := r.Read(head[:1]); err != nil {
		t.Fatal(err)
	}
	n := 1
	for b := head[0]; b&0x80 == 0; b <<= 1 {
		n++
	}
	if _, err := r.Read(head[1:n]); err != nil {
		t.Fatal(err)
	}
	var v uint64
	for j := 0; j < n; j++ {
		v = v<<8 | uint64(head[j])
	}
	v &^= uint64(1) << (7 * uint(n))
	return data[i+len(id)+n : i+len(id)+n+int(v)]
}

func TestAttachCuesRejectsFabricatedPoints(t *testing.T) {
	ix := FromSamples([]demux.Sample{sample(1, 0, true, 100), sample(1, 40, true, 200)})
	points := []CuePoint{
		{Tick: 0, Track: 1, ClusterPosition: 10},
		{Tick: 40, Track: 1, ClusterPosition: 9999},
		{Tick: 80, Track: 1, ClusterPosition: 10},
	}
	accepted := ix.AttachCues(points, 0, []int64{10})
	if accepted != 1 {
		t.Fatalf("accepted=%d discarded=%d", accepted, ix.DiscardedCues)
	}
	if ix.DiscardedCues != 2 {
		t.Fatalf("discarded=%d", ix.DiscardedCues)
	}
	if !ix.Entries[0].FromCues || ix.Entries[1].FromCues {
		t.Fatalf("corroboration wrong: %+v", ix.Entries)
	}
	if ix.Rebuilt {
		t.Fatal("usable cues should not mark the index rebuilt")
	}
}

func TestValidate(t *testing.T) {
	ix := FromSamples([]demux.Sample{sample(1, 10, true, 100)})
	if err := ix.Validate(1000); err != nil {
		t.Fatal(err)
	}
	bad := Index{Entries: []Entry{{Track: 1, Tick: 0, SourceOffset: 5000}}}
	err := bad.Validate(1000)
	if !errors.Is(err, fault.ErrMalformed) || fault.StageOf(err) != fault.StageIndex {
		t.Fatalf("got %v", err)
	}
	regress := Index{Entries: []Entry{{Track: 1, Tick: 10, SourceOffset: 1}, {Track: 1, Tick: 5, SourceOffset: 2}}}
	if err := regress.Validate(1000); err == nil {
		t.Fatal("regressing ticks must fail validation")
	}
}
