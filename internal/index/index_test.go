package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediascan/internal/clock"
	"mediascan/internal/fixture"
)

// writeFragmented writes a two-track fragmented MP4 with one fragment per
// 100 ms segment and returns its path.
func writeFragmented(t *testing.T, nSeg int, truncated bool) string {
	t.Helper()
	tracks := []fixture.MP4Track{
		{ID: 1, Handler: "vide", Timescale: 90000, Width: 1280, Height: 720,
			Primaries: 1, Transfer: 1, Matrix: 1},
		{ID: 2, Handler: "soun", Timescale: 48000, Channels: 6, SampleRate: 48000,
			ChanConfig: 6, ObjectType: 0x40},
	}
	var frags []fixture.MP4Fragment
	for i := 0; i < nSeg; i++ {
		for _, tr := range tracks {
			dur := uint32(9000)
			if tr.Handler == "soun" {
				dur = 4800
			}
			samples := []fixture.MP4Sample{
				{Duration: dur, Data: fixture.SampleData(tr.ID, i, 0), Sync: true},
				{Duration: dur, Data: fixture.SampleData(tr.ID, i, 1), Sync: false},
			}
			// A fragment holds one second of media so tfdt advances by
			// dur*samples in the track's own timescale.
			base := uint64(i) * 90000
			if tr.Handler == "soun" {
				base = uint64(i) * 48000
			}
			frags = append(frags, fixture.MP4Fragment{TrackID: tr.ID, BaseDecodeTime: base, Samples: samples})
		}
	}
	data := fixture.BuildFragmentedMP4(tracks, frags)
	if truncated {
		data = data[:len(data)-20]
	}
	path := filepath.Join(t.TempDir(), "sample.mp4")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenAndBuildFragmented(t *testing.T) {
	path := writeFragmented(t, 10, false)
	src, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if len(src.tracks) != 2 {
		t.Fatalf("tracks=%d", len(src.tracks))
	}
	for _, ts := range src.tracks {
		if len(ts.samples) != 20 {
			t.Fatalf("track %d samples=%d", ts.id, len(ts.samples))
		}
	}
	idx, err := src.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := src.Verify(idx)
	if !rep.Valid {
		t.Fatalf("verify failed: %+v", rep)
	}
	if rep.Points == 0 || rep.SyncPoints != rep.Points {
		t.Fatalf("points=%d sync=%d", rep.Points, rep.SyncPoints)
	}
}

func TestIndexTimesReconciledAgainstRepair(t *testing.T) {
	path := writeFragmented(t, 8, false)
	src, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	// Build a repair map matching the one-second fragments: each tfdt base
	// advances by i*90000 ticks on the video track.
	var obs []clock.Observed
	for i := 0; i < 8; i++ {
		obs = append(obs, clock.Observed{
			ID:       "seg-" + itoa(i),
			Start:    uint64(i) * 90000,
			HasStart: true,
			Duration: 1_000_000_000,
			Valid:    true,
		})
	}
	m := clock.Build(obs, clock.Options{})
	idx, err := src.Build(&m)
	if err != nil {
		t.Fatal(err)
	}
	// Every video point must reference an available repair entry.
	for _, tr := range idx.Tracks {
		if tr.ID != 1 {
			continue
		}
		for _, p := range tr.Points {
			if p.SegmentID == "" {
				t.Fatal("point has no segment id")
			}
			e, ok := m.Lookup(p.SegmentID)
			if !ok || !e.Available {
				t.Fatalf("point %d maps to unavailable %q", p.TimeNs, p.SegmentID)
			}
		}
	}
}

func TestTruncatedSourceFlagged(t *testing.T) {
	path := writeFragmented(t, 6, true)
	src, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if !src.truncated {
		t.Fatal("expected the truncated tail to be detected")
	}
	idx, err := src.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := src.Verify(idx)
	if rep.Valid {
		t.Fatalf("truncated source must not verify: %+v", rep)
	}
	if len(rep.Problems) == 0 {
		t.Fatal("expected a problem description")
	}
}

func TestVerifyDetectsMisalignedPoint(t *testing.T) {
	path := writeFragmented(t, 4, false)
	src, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	idx, err := src.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt one offset to land mid-sample.
	idx.Tracks[0].Points[0].Offset++
	rep := src.Verify(idx)
	if rep.Valid || rep.Mismatches == 0 {
		t.Fatalf("misaligned point must fail: %+v", rep)
	}
}

func TestOpenRejectsNonMedia(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.bin")
	if err := os.WriteFile(path, []byte("not an mp4 at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrNoSamples) && !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestFlattenOrder(t *testing.T) {
	path := writeFragmented(t, 5, false)
	src, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	idx, err := src.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	flat := idx.Flatten()
	if len(flat) == 0 {
		t.Fatal("no points")
	}
	for i := 1; i < len(flat); i++ {
		if flat[i].TimeNs < flat[i-1].TimeNs {
			t.Fatalf("flatten not time ordered at %d", i)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
