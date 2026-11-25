package ingest

import (
	"testing"

	"fieldcut/internal/clock"
)

func TestBuildAndReadFragmentedMP4(t *testing.T) {
	track := &MP4Track{
		TrackID: 2, TimeScale: 30000,
		Color: ColorInfo{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709", HasMetadata: true},
		Samples: []MP4Sample{
			{PTS: 0, Duration: 33_333_333, Data: []byte("keyframe-bytes"), Sync: true},
			{PTS: 33333333, Duration: 33_333_333, Data: []byte("p-frame")},
			{PTS: 66666666, Duration: 33_333_333, Data: []byte("p-frame-2")},
		},
	}
	data, err := BuildFragmentedMP4(track)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadFragmentedMP4(data, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrackID != 2 || got.TimeScale != 30000 || len(got.Samples) != 3 {
		t.Fatalf("bad track: %+v", got)
	}
	if string(got.Samples[1].Data) != "p-frame" {
		t.Fatalf("sample payload mismatch: %q", got.Samples[1].Data)
	}
	if got.Samples[1].PTS != 33_333_333 {
		t.Fatalf("PTS=%d", got.Samples[1].PTS)
	}
	if got.Color.Primaries != "bt709" || !got.Color.HasMetadata {
		t.Fatalf("color not preserved: %+v", got.Color)
	}
}

func TestReadTruncatedMDAT(t *testing.T) {
	track := &MP4Track{TrackID: 9, TimeScale: 48000, Samples: []MP4Sample{
		{Duration: 24000, Data: make([]byte, 128)},
		{Duration: 24000, Data: make([]byte, 128)},
	}}
	data, err := BuildFragmentedMP4(track)
	if err != nil {
		t.Fatal(err)
	}
	// Remove the final payload byte while preserving declared box/sample sizes.
	damaged := data[:len(data)-1]
	_, err = ReadFragmentedMP4(damaged, 9)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	ce, ok := clock.AsError(err)
	if !ok || ce.Code != clock.CodeTruncatedMP4 || ce.TrackID != 9 {
		t.Fatalf("expected structured truncated_mp4 track 9, got %v", err)
	}
}

func TestReadTruncatedMOOF(t *testing.T) {
	track := &MP4Track{TrackID: 5, TimeScale: 30000, Samples: []MP4Sample{
		{Duration: 33_333_333, Data: []byte("sample")},
	}}
	data, err := BuildFragmentedMP4(track)
	if err != nil {
		t.Fatal(err)
	}
	// Cut well inside the first moof; its declared size now exceeds the file.
	cut := len(data) / 3
	if cut > 80 {
		cut = 80
	}
	_, err = ReadFragmentedMP4(data[:cut], 5)
	if err == nil {
		t.Fatal("expected moof truncation error")
	}
	if ce, ok := clock.AsError(err); !ok || ce.Code != clock.CodeTruncatedMP4 {
		t.Fatalf("unexpected error: %v", err)
	}
}
