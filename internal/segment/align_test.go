package segment

import (
	"errors"
	"testing"

	"mediad/internal/codec"
	"mediad/internal/mediaerr"
)

func keyFrame(r int, pts int64) Frame {
	return Frame{Rendition: r, PTS: pts, DTS: pts, Keyframe: true, Codec: codec.CodecH264, Payload: []byte{0, 0, 0, 1, 0x65}}
}

func pFrame(r int, pts int64) Frame {
	return Frame{Rendition: r, PTS: pts, DTS: pts, Keyframe: false, Codec: codec.CodecH264, Payload: []byte{0, 0, 0, 1, 0x41}}
}

func TestAlignTracksAtCommonIDRsWithin20ms(t *testing.T) {
	t0 := Track{ID: "low", Frames: []Frame{
		keyFrame(0, 0), pFrame(0, 3003),
		keyFrame(0, 90000), pFrame(0, 93003),
		keyFrame(0, 180000),
	}}
	t1 := Track{ID: "high", Frames: []Frame{
		keyFrame(1, 900), pFrame(1, 3903),
		keyFrame(1, 90900), pFrame(1, 93903),
		keyFrame(1, 180900),
	}}
	a, err := AlignTracks([]Track{t0, t1})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Segments) != 2 {
		t.Fatalf("segments=%d want 2", len(a.Segments))
	}
	if a.MaxSkewMS > 20 {
		t.Fatalf("skew %.3fms exceeds 20ms", a.MaxSkewMS)
	}
	if a.Segments[0].DurationMS != 1000 {
		t.Fatalf("duration=%dms want 1000", a.Segments[0].DurationMS)
	}
}

func TestAlignDefersWhenEarlyCommonKeyMissing(t *testing.T) {
	// Rendition one lacks the PTS=0 IDR but has later common IDRs. Alignment
	// must defer the first split rather than cutting a non-common boundary.
	t0 := Track{Frames: []Frame{keyFrame(0, 0), keyFrame(0, 90000), keyFrame(0, 180000)}}
	t1 := Track{Frames: []Frame{keyFrame(1, 90000), keyFrame(1, 180000)}}
	a, err := AlignTracks([]Track{t0, t1})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Boundaries) != 2 || a.Boundaries[0].PTS != 90000 {
		t.Fatalf("boundaries=%+v want deferred start at 90000", a.Boundaries)
	}
	if len(a.Segments) != 1 {
		t.Fatalf("segments=%d want one closed later segment", len(a.Segments))
	}
}

func TestAlignFailsWhenNoCommonKeyframe(t *testing.T) {
	t0 := Track{Frames: []Frame{keyFrame(0, 0), keyFrame(0, 90000)}}
	t1 := Track{Frames: []Frame{keyFrame(1, 50000), keyFrame(1, 140000)}}
	_, err := AlignTracks([]Track{t0, t1})
	var se *mediaerr.Error
	if !errors.As(err, &se) || se.Kind != mediaerr.KindNoCommonKeyframe {
		t.Fatalf("err=%v want KindNoCommonKeyframe", err)
	}
	if code := mediaerr.ExitCode(err); code != 2 {
		t.Fatalf("exit code=%d want 2", code)
	}
}

func TestAlignDoesNotFabricateFixedFrameTimestamps(t *testing.T) {
	// Irregular VFR gaps must survive into chunk frame PTS values unchanged.
	irregular := []int64{0, 3003, 7777, 12012, 90000, 94505, 180000}
	var frames []Frame
	for i, pts := range irregular {
		f := pFrame(0, pts)
		if i == 0 || pts == 90000 || pts == 180000 {
			f = keyFrame(0, pts)
		}
		frames = append(frames, f)
	}
	a, err := AlignTracks([]Track{{Frames: frames}, {Frames: []Frame{keyFrame(1, 0), keyFrame(1, 90000), keyFrame(1, 180000)}}})
	if err != nil {
		t.Fatal(err)
	}
	got := a.Segments[0].Chunks[0].Frames
	if len(got) != 4 {
		t.Fatalf("frames=%d want 4 before 90000 boundary", len(got))
	}
	for i := range got {
		if got[i].PTS != irregular[i] {
			t.Fatalf("PTS %d=%d want original %d", i, got[i].PTS, irregular[i])
		}
	}
}
