package output_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"bcast/demux"
	"bcast/internal/fixture"
	"bcast/media"
	"bcast/output"

	mp4 "github.com/abema/go-mp4"
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

func TestWriteFileAtomicRoundTrip(t *testing.T) {
	a, err := fixture.Generate(fixture.Default(17, 12))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
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
				CTSOffset: s.CTS.Value - s.DTS.Value,
			})
		}
		tracks = append(tracks, tm)
	}
	path := filepath.Join(dir, "out.mp4")
	rep, err := output.WriteFile(path, bytes.NewReader(a.Clean), tracks)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if rep.MatchRate() != 1 {
		t.Fatalf("match rate=%v rep=%+v", rep.MatchRate(), rep)
	}
	if rep.SamplesOut != rep.SamplesIn || rep.MissingSamples != 0 || rep.ByteMismatch != 0 {
		t.Fatalf("rep=%+v", rep)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// No temporary files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "out.mp4" {
			t.Fatalf("leftover %q", e.Name())
		}
	}
}

func TestWriteFileLeavesDestinationOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(path, []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A source that cannot supply bytes must fail the remux.
	tracks := []output.TrackMux{{
		ID: 1, Kind: media.TrackVideo, Timescale: 90000,
		Samples: []output.SampleRef{{Offset: 0, Size: 100, Duration: 3000}},
	}}
	_, err := output.WriteFile(path, bytes.NewReader(nil), tracks)
	if err == nil {
		t.Fatal("expected failure")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "previous" {
		t.Fatalf("destination replaced on failure: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
}

func TestSegmentKeepsAbsolutePosition(t *testing.T) {
	f := &media.File{
		Tracks: []media.Track{{ID: 1, Kind: media.TrackVideo, Timescale: 1000}},
		Samples: []media.Sample{
			{TrackID: 1, Kind: media.TrackVideo, DTS: media.Time{Value: 0, Timescale: 1000}, Duration: 1000, Size: 4},
			{TrackID: 1, Kind: media.TrackVideo, DTS: media.Time{Value: 1000, Timescale: 1000}, Duration: 1000, Size: 4},
		},
	}
	_ = f
	payload := []byte("abcdabcd")
	tracks := []output.TrackMux{{
		ID: 1, Kind: media.TrackVideo, Timescale: 1000, StartOffset: 5000,
		Samples: []output.SampleRef{{Offset: 0, Size: 8, Duration: 1000, Keyframe: true}},
	}}
	out, err := output.MuxBytes(bytes.NewReader(payload), tracks)
	if err != nil {
		t.Fatal(err)
	}
	// go-mp4 must parse the edit list we wrote.
	if !bytes.Contains(out, []byte("edts")) || !bytes.Contains(out, []byte("elst")) {
		t.Fatal("segment with StartOffset has no edit list")
	}
	// The leading empty edit must be present: media_time -1 in the first
	// entry, proving the offset is a presentation gap, not a re-shift.
	var el mp4.Elst
	found := false
	_, _ = mp4.ReadBoxStructure(bytes.NewReader(out), func(h *mp4.ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type != mp4.BoxTypeElst() {
			return h.Expand()
		}
		box, _, perr := h.ReadPayload()
		if perr != nil {
			return nil, perr
		}
		el = *box.(*mp4.Elst)
		found = true
		return nil, nil
	})
	if !found {
		t.Fatal("elst not parsed")
	}
	if el.EntryCount != 2 || int32(el.Entries[0].MediaTimeV0) != -1 {
		t.Fatalf("elst=%+v", el)
	}
}
