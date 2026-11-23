package silence_test

import (
	"bytes"
	"testing"

	"bcast/demux"
	"bcast/internal/fixture"
	"bcast/media"
	"bcast/silence"
)

func detect(t *testing.T, data []byte) *silence.Report {
	t.Helper()
	f, err := demux.Parse(bytes.NewReader(data), int64(len(data)), "in")
	if err != nil && len(f.Samples) == 0 {
		t.Fatalf("parse: %v", err)
	}
	return silence.Detect(&f.File, bytes.NewReader(data), 4800, 500)
}

func TestDetectSilentWindows(t *testing.T) {
	a, err := fixture.Generate(fixture.Default(17, 12))
	if err != nil {
		t.Fatal(err)
	}
	rep := detect(t, a.Clean)
	if rep.Status != silence.StatusOK {
		t.Fatalf("status=%s", rep.Status)
	}
	if len(rep.Windows) != 120 { // 12s / 0.1s
		t.Fatalf("windows=%d", len(rep.Windows))
	}
	// Segments 2 and 9 are silent: 10 windows each.
	if rep.SilentWindows != 20 {
		t.Fatalf("silent=%d", rep.SilentWindows)
	}
	for _, w := range rep.Windows {
		seg := w.Index / 10
		wantSilent := seg == 2 || seg == 9
		if w.Silent != wantSilent {
			t.Fatalf("window %d silent=%v want %v", w.Index, w.Silent, wantSilent)
		}
	}
}

func TestTruncatedTailNotSilent(t *testing.T) {
	a, err := fixture.Generate(fixture.Default(17, 12))
	if err != nil {
		t.Fatal(err)
	}
	rep := detect(t, a.Truncated)
	// Windows past the truncation point lack data: unknown, never silent.
	tail := rep.Windows[len(rep.Windows)-1]
	if tail.Status != silence.StatusCrossesDamage {
		t.Fatalf("tail status=%s", tail.Status)
	}
	if tail.Silent {
		t.Fatal("unknown window judged silent")
	}
}

func TestNoAudioTrack(t *testing.T) {
	f := &media.File{Tracks: []media.Track{{ID: 1, Kind: media.TrackVideo, Timescale: 90000}}}
	rep := silence.Detect(f, bytes.NewReader(nil), 4800, 500)
	if rep.Status != silence.StatusNoAudioTrack {
		t.Fatalf("status=%s", rep.Status)
	}
}

func TestTimestampRegression(t *testing.T) {
	f := &media.File{
		Tracks: []media.Track{{ID: 2, Kind: media.TrackAudio, Timescale: 48000}},
		Samples: []media.Sample{
			{TrackID: 2, Kind: media.TrackAudio, DTS: media.Time{Value: 4800, Timescale: 48000}, Duration: 4800},
			{TrackID: 2, Kind: media.TrackAudio, DTS: media.Time{Value: 0, Timescale: 48000}, Duration: 4800},
		},
	}
	rep := silence.Detect(f, bytes.NewReader(nil), 4800, 500)
	if rep.Status != silence.StatusTSRegression {
		t.Fatalf("status=%s", rep.Status)
	}
}

func TestPartialWindowIsUnknownNotSilent(t *testing.T) {
	// One PCM sample covers only the first window; the declared duration is
	// longer. The second window is uncovered, so it must be unknown.
	f := &media.File{
		Tracks: []media.Track{{ID: 2, Kind: media.TrackAudio, Timescale: 48000, Duration: 9600}},
		Samples: []media.Sample{
			{TrackID: 2, Kind: media.TrackAudio, DTS: media.Time{Value: 0, Timescale: 48000},
				Duration: 4800, Offset: 0, Size: 9600},
		},
	}
	src := bytes.NewReader(make([]byte, 9600)) // all zero: "silent" if judged
	rep := silence.Detect(f, src, 4800, 500)
	if len(rep.Windows) != 2 {
		t.Fatalf("windows=%d", len(rep.Windows))
	}
	if !rep.Windows[0].Silent || rep.Windows[0].Status != silence.StatusOK {
		t.Fatalf("covered window: %+v", rep.Windows[0])
	}
	if rep.Windows[1].Silent || rep.Windows[1].Status == silence.StatusOK {
		t.Fatalf("uncovered window judged silent: %+v", rep.Windows[1])
	}
	if rep.UnknownWindows != 1 {
		t.Fatalf("unknown=%d", rep.UnknownWindows)
	}
}

func TestSampleCrossingDamageIsUnknown(t *testing.T) {
	// The audio sample starts before the damaged range but its payload
	// overlaps it; the whole window is unknown.
	f := &media.File{
		Tracks: []media.Track{{ID: 2, Kind: media.TrackAudio, Timescale: 48000, Duration: 4800}},
		Samples: []media.Sample{
			{TrackID: 2, Kind: media.TrackAudio, DTS: media.Time{Value: 0, Timescale: 48000},
				Duration: 4800, Offset: 0, Size: 9600},
		},
		Damaged: []media.Range{{Start: 9000, End: 10000, Reason: "tail"}},
	}
	src := bytes.NewReader(make([]byte, 9600))
	rep := silence.Detect(f, src, 4800, 500)
	if rep.SilentWindows != 0 || rep.Windows[0].Silent {
		t.Fatal("damaged sample judged silent")
	}
	if rep.Windows[0].Status != silence.StatusCrossesDamage {
		t.Fatalf("status=%s", rep.Windows[0].Status)
	}
}

func TestHoleBetweenSamplesIsGap(t *testing.T) {
	// Samples cover [0,2400) and [4800,7200), leaving a hole; the second
	// window is half present and must be a gap, not silence.
	f := &media.File{
		Tracks: []media.Track{{ID: 2, Kind: media.TrackAudio, Timescale: 48000, Duration: 9600}},
		Samples: []media.Sample{
			{TrackID: 2, Kind: media.TrackAudio, DTS: media.Time{Value: 0, Timescale: 48000}, Duration: 2400, Size: 4800},
			{TrackID: 2, Kind: media.TrackAudio, DTS: media.Time{Value: 4800, Timescale: 48000}, Duration: 2400, Size: 4800, Offset: 4800},
		},
	}
	src := bytes.NewReader(make([]byte, 9600))
	rep := silence.Detect(f, src, 4800, 500)
	if rep.Windows[1].Silent {
		t.Fatal("gap window judged silent")
	}
	if rep.Windows[1].Status != silence.StatusGap {
		t.Fatalf("status=%s", rep.Windows[1].Status)
	}
}
