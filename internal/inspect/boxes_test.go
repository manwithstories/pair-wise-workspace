package inspect

import (
	"bytes"
	"testing"

	"mediacheck/internal/model"
)

// buildBox is a local minimal box writer mirroring the fixture format.
func buildBox(typ string, payload []byte, children ...[]byte) []byte {
	var body bytes.Buffer
	body.Write(payload)
	for _, c := range children {
		body.Write(c)
	}
	size := uint32(8 + body.Len())
	hdr := make([]byte, 8)
	hdr[0] = byte(size >> 24)
	hdr[1] = byte(size >> 16)
	hdr[2] = byte(size >> 8)
	hdr[3] = byte(size)
	copy(hdr[4:8], []byte(typ))
	return append(hdr, body.Bytes()...)
}

func TestWalkBoxesHealthy(t *testing.T) {
	stbl := buildBox("stbl", nil)
	minf := buildBox("minf", nil, stbl)
	mdia := buildBox("mdia", nil, buildBox("mdhd", nil), minf)
	trak := buildBox("trak", nil, mdia)
	moov := buildBox("moov", nil, buildBox("mvhd", nil), trak)
	data := append(buildBox("ftyp", []byte("isom")), moov...)
	data = append(data, buildBox("mdat", make([]byte, 32))...)

	rep, err := WalkBoxes(bytes.NewReader(data), int64(len(data)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Faults) != 0 {
		t.Fatalf("healthy input must have no faults, got %v", rep.Faults)
	}
	if rep.MDATBytes != 32 {
		t.Fatalf("mdat payload bytes=%d want 32", rep.MDATBytes)
	}
}

func TestWalkBoxesTruncated(t *testing.T) {
	good := buildBox("ftyp", []byte("isom"))
	mdat := buildBox("mdat", make([]byte, 64))
	// Declare 64 payload bytes but supply only 20.
	truncated := mdat[:8+20]
	data := append(good, truncated...)
	rep, err := WalkBoxes(bytes.NewReader(data), int64(len(data)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range rep.Faults {
		if f.Kind == model.KindTruncatedBox {
			found = true
			// The fault carries the box offset and the recoverable end
			// boundary so downstream repair can scope the segment.
			if f.StartMs < 0 || f.EndMs < f.StartMs {
				t.Fatal("truncated box fault must carry valid boundaries")
			}
		}
	}
	if !found {
		t.Fatal("truncated box not detected")
	}
}

func TestAnalyzeTrackKeyframeViolation(t *testing.T) {
	samples := []Sample{}
	for ms := int64(0); ms < 6000; ms += 100 {
		samples = append(samples, Sample{
			Track: "video", StartMs: ms, DurMs: 100,
			IsSync: ms == 0 || ms == 3000, // 3000ms gap > 2500 policy
		})
	}
	rep := AnalyzeTrack(samples, KeyframePolicy{MaxIntervalMs: 2500})
	if len(rep.Violations) != 1 {
		t.Fatalf("want 1 keyframe violation, got %d", len(rep.Violations))
	}
	v := rep.Violations[0]
	if v.PrevSync != 0 || v.NextSync != 3000 || v.GapMs != 3000 {
		t.Fatalf("violation boundaries wrong: %+v", v)
	}
}

func TestAnalyzeTrackMissingSyncAndCorrupt(t *testing.T) {
	samples := []Sample{
		{Track: "v", StartMs: 0, DurMs: 100},
		{Track: "v", StartMs: 100, DurMs: 100, Corrupt: true},
		{Track: "v", StartMs: 200, DurMs: 100, Corrupt: true},
		{Track: "v", StartMs: 300, DurMs: 100},
	}
	rep := AnalyzeTrack(samples, DefaultKeyframePolicy())
	if !rep.MissingSync {
		t.Fatal("track without sync sample must be flagged")
	}
	if len(rep.CorruptSpans) != 1 || rep.CorruptSpans[0].StartMs != 100 {
		t.Fatalf("corrupt span wrong: %+v", rep.CorruptSpans)
	}
	if rep.NegativeDur != 0 {
		t.Fatal("no negative durations expected")
	}
}

func TestTrackFaultsStableIDs(t *testing.T) {
	samples := []Sample{{Track: "v", StartMs: 0, DurMs: 100}}
	rep := AnalyzeTrack(samples, DefaultKeyframePolicy())
	next := 10
	faults := TrackFaults(rep, func() int { id := next; next++; return id })
	if len(faults) != 1 || faults[0].ID != 10 || faults[0].Kind != model.KindMissingKeyframe {
		t.Fatalf("fault id/kind wrong: %+v", faults)
	}
}
