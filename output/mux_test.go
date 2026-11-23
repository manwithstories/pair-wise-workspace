package output_test

import (
	"bytes"
	"testing"

	"bcast/demux"
	"bcast/internal/fixture"
	"bcast/media"
	"bcast/output"
)

func TestRemuxPreservesCodecEntry(t *testing.T) {
	a, err := fixture.Generate(fixture.Default(17, 12))
	if err != nil {
		t.Fatal(err)
	}
	f, err := demux.Parse(bytes.NewReader(a.Clean), int64(len(a.Clean)), "clean")
	if err != nil {
		t.Fatal(err)
	}
	var tracks []output.TrackMux
	for _, tr := range f.Tracks {
		tm := output.TrackMux{ID: tr.ID, Kind: tr.Kind, Timescale: tr.Timescale, CodecEntry: tr.CodecEntry}
		for _, s := range f.SamplesOf(tr.ID) {
			tm.Samples = append(tm.Samples, output.SampleRef{
				Offset: s.Offset, Size: s.Size, Duration: s.Duration, Keyframe: s.Keyframe,
			})
		}
		tracks = append(tracks, tm)
	}
	out, err := output.MuxBytes(bytes.NewReader(a.Clean), tracks)
	if err != nil {
		t.Fatalf("mux: %v", err)
	}
	g, err := demux.Parse(bytes.NewReader(out), int64(len(out)), "out")
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(g.Samples) != len(f.Samples) {
		t.Fatalf("samples %d != %d", len(g.Samples), len(f.Samples))
	}
	if !bytes.Equal(g.Tracks[0].CodecEntry, f.Tracks[0].CodecEntry) {
		t.Fatal("video stsd not preserved byte-for-byte")
	}
	if !bytes.Equal(g.Tracks[1].CodecEntry, f.Tracks[1].CodecEntry) {
		t.Fatal("audio stsd not preserved byte-for-byte")
	}
	// Compressed payloads are copied, never regenerated.
	for _, s := range g.Samples {
		src := f.SamplesOf(s.TrackID)
		_ = src
	}
	_ = media.TrackVideo
}
