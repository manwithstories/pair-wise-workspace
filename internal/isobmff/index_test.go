package isobmff

import (
	"testing"

	"fieldqc/internal/fault"
	"fieldqc/internal/fixture"
)

func testTracks() []fixture.MP4Track {
	return []fixture.MP4Track{
		{ // 60 s of 25 fps video, keyframe every 75 frames (3 s)
			ID: 1, Kind: "video", Timescale: 90000,
			Deltas: []uint32{3600}, Counts: []uint32{1500},
			Keyframes:    []uint32{1, 76, 151, 226, 301, 376, 451, 526, 601, 676, 751, 826, 901, 976, 1051, 1126, 1201, 1276, 1351, 1426},
			SampleSize:   40,
			ChunkSamples: 250,
			MediaTime:    -1,
		},
		{ // 60 s of audio with a non-zero start of 0.5 s
			ID: 2, Kind: "audio", Timescale: 48000,
			Deltas: []uint32{1024}, Counts: []uint32{2813},
			SampleSize:   8,
			ChunkSamples: 469,
			MediaTime:    24000,
		},
	}
}

func spansWithCode(res *Result, code fault.Code) []fault.Span {
	var out []fault.Span
	for _, s := range res.Faults.Spans() {
		if s.Code == code {
			out = append(out, s)
		}
	}
	return out
}

func TestAnalyzeClean(t *testing.T) {
	mp := fixture.BuildMP4(testTracks())
	res, err := Analyze("clean.mp4", mp.Data)
	if err != nil {
		t.Fatal(err)
	}
	if res.Faults.Len() != 0 {
		t.Fatalf("unexpected faults: %v", res.Faults.Spans())
	}
	if len(res.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(res.Tracks))
	}
	v, a := res.Tracks[0], res.Tracks[1]
	if v.Kind != "video" || v.SampleCount != 1500 {
		t.Fatalf("video track: %+v", v)
	}
	if got := v.MaxKeyInt.Millis(); got != 3000 {
		t.Fatalf("max keyframe interval = %v ms", got)
	}
	if a.Kind != "audio" || a.StartDTS.Millis() != 500 {
		t.Fatalf("audio track: %+v", a)
	}
	if a.Duration.Millis() < 59000 {
		t.Fatalf("audio duration too short: %v", a.Duration)
	}
}

func TestTruncatedMdatMarksOnlyAffectedRange(t *testing.T) {
	mp := fixture.BuildMP4(testTracks())
	cut := mp.DataStart[1] + 4096 // inside the audio track's mdat region
	res, err := Analyze("cut.mp4", mp.Data[:cut])
	if err != nil {
		t.Fatal(err)
	}
	// Video track must still be fully indexed.
	v := res.Tracks[0]
	if v.SampleCount != 1500 || v.Keyframes != 20 {
		t.Fatalf("video track damaged by audio truncation: %+v", v)
	}
	for _, s := range res.Faults.Spans() {
		if s.Track == v.Name {
			t.Fatalf("fault leaked onto intact video track: %v", s)
		}
	}
	trunc := spansWithCode(res, fault.CodeTruncatedBox)
	if len(trunc) == 0 {
		t.Fatal("no TRUNCATED_BOX span reported")
	}
	found := false
	for _, s := range trunc {
		if s.Track == res.Tracks[1].Name && s.Start.Sign() > 0 && s.End.Cmp(s.Start) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no localized span on audio track: %v", trunc)
	}
}

func TestTruncatedBoxInsideMoovKeepsOtherTracks(t *testing.T) {
	mp := fixture.BuildMP4(testTracks())
	cut := mp.DataStart[1] - 512 // inside the audio trak's sample table
	res, err := Analyze("moovcut.mp4", mp.Data[:cut])
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tracks) != 2 {
		t.Fatalf("tracks = %d", len(res.Tracks))
	}
	v := res.Tracks[0]
	if v.SampleCount != 1500 || v.Keyframes != 20 || v.MaxKeyInt.Millis() != 3000 {
		t.Fatalf("video sample table not recovered: %+v", v)
	}
	trunc := spansWithCode(res, fault.CodeTruncatedBox)
	if len(trunc) == 0 {
		t.Fatal("expected TRUNCATED_BOX span")
	}
}

func TestTimestampJumpDetected(t *testing.T) {
	tracks := testTracks()
	tracks[0].Deltas = []uint32{3600, 36000, 3600} // one 10x jump
	tracks[0].Counts = []uint32{700, 1, 799}
	tracks[0].Keyframes = []uint32{1, 701}
	mp := fixture.BuildMP4(tracks)
	res, err := Analyze("jump.mp4", mp.Data)
	if err != nil {
		t.Fatal(err)
	}
	jumps := spansWithCode(res, fault.CodeTimestampJump)
	if len(jumps) != 2 { // jump up to 10x delta and back down to normal
		t.Fatalf("jumps = %v", jumps)
	}
	if got := jumps[0].Start.Millis(); got < 27900 || got > 28100 {
		t.Fatalf("jump located at %v ms, want ~28000 ms", got)
	}
}

func TestEditListOutOfRange(t *testing.T) {
	tracks := testTracks()
	tracks[1].MediaTime = 48000 * 3600 // 1 h into a 60 s track
	mp := fixture.BuildMP4(tracks)
	res, err := Analyze("elst.mp4", mp.Data)
	if err != nil {
		t.Fatal(err)
	}
	edits := spansWithCode(res, fault.CodeEditRange)
	if len(edits) != 1 || edits[0].Track != res.Tracks[1].Name {
		t.Fatalf("edit faults = %v", edits)
	}
}
