package audio

import (
	"testing"

	"relaycheck/internal/media"
)

func segRange(t *testing.T) media.TimeRange {
	c, f := media.NewClock(48000)
	if f != nil {
		t.Fatal(f)
	}
	r, f := c.Range(0, FramesPerSegment)
	if f != nil {
		t.Fatal(f)
	}
	return r
}

func TestAllSourceRatesConvertTo48kStereo(t *testing.T) {
	rng := segRange(t)
	for _, rate := range SourceRates {
		for _, ch := range []int{1, 2} {
			frames := rate / 50 // 20 ms
			src := TonePCM(rate, ch, frames, int64(rate))
			out, f := Convert(src, Options{
				TrackID: 2, TargetRate: 48000, OutputChannels: 2,
				ExpectedFrames: FramesPerSegment, SegmentRange: rng,
			})
			if f != nil {
				t.Fatalf("rate=%d ch=%d: %v", rate, ch, f)
			}
			if out.Rate != 48000 || out.Channels != 2 || out.Frames() != FramesPerSegment {
				t.Fatalf("rate=%d ch=%d -> rate=%d ch=%d frames=%d", rate, ch, out.Rate, out.Channels, out.Frames())
			}
		}
	}
}

func TestMonoMapDuplicatesCenter(t *testing.T) {
	src := TonePCM(48000, 1, 32, 3)
	m, f := StereoMap(1)
	if f != nil {
		t.Fatal(f)
	}
	out, f := MapChannels(src, m, 1)
	if f != nil {
		t.Fatal(f)
	}
	for i := 0; i < 32; i++ {
		if out.Samples[2*i] != out.Samples[2*i+1] {
			t.Fatalf("mono must duplicate at frame %d", i)
		}
	}
}

func TestEmptyChannelFaultHasTrack(t *testing.T) {
	rng := segRange(t)
	_, f := Convert(&PCM{Rate: 48000, Channels: 1, Samples: nil}, Options{TrackID: 9, SegmentRange: rng})
	if f == nil || f.Code != media.CodeEmptyChannel || f.TrackID != 9 || f.Range == nil {
		t.Fatalf("empty channel fault malformed: %+v", f)
	}
	if f.ExitCode() != 2 {
		t.Fatalf("exit=%d want 2", f.ExitCode())
	}
	if _, f := StereoMap(3); f == nil || f.Code != media.CodeEmptyChannel {
		t.Fatalf("3-channel map should be explicit empty/missing mapping: %v", f)
	}
}

func TestIncompleteRangeFault(t *testing.T) {
	rng := segRange(t)
	src := TonePCM(48000, 2, 100, 1)
	_, f := Convert(src, Options{TrackID: 4, ExpectedFrames: 960, SegmentRange: rng})
	if f == nil || f.Code != media.CodeIncompleteRange {
		t.Fatalf("want incomplete_range, got %v", f)
	}
	if f.TrackID != 4 || f.Range == nil {
		t.Fatalf("fault missing track/range: %+v", f)
	}
}

func TestGapSilenceZeroFills(t *testing.T) {
	rng := segRange(t)
	src := TonePCM(48000, 2, FramesPerSegment, 5)
	out, f := Convert(src, Options{
		TrackID: 1, ExpectedFrames: FramesPerSegment, SegmentRange: rng,
		Gaps: []FrameGap{{StartFrame: 10, EndFrame: 20}}, GapPolicy: GapSilence,
	})
	if f != nil {
		t.Fatal(f)
	}
	for fr := 10; fr < 20; fr++ {
		if out.Samples[2*fr] != 0 || out.Samples[2*fr+1] != 0 {
			t.Fatalf("gap frame %d not silenced", fr)
		}
	}
}

func TestGapMarkUnrecoverableErrors(t *testing.T) {
	rng := segRange(t)
	src := TonePCM(48000, 2, FramesPerSegment, 5)
	_, f := Convert(src, Options{
		TrackID: 6, ExpectedFrames: FramesPerSegment, SegmentRange: rng,
		Gaps:      []FrameGap{{StartFrame: 30, EndFrame: 36, Range: rng}},
		GapPolicy: GapMarkUnrecoverable,
	})
	if f == nil || f.Code != media.CodeUnrecoverableGap || f.TrackID != 6 || f.Range == nil {
		t.Fatalf("unrecoverable gap fault malformed: %+v", f)
	}
}

func TestDetectClipping(t *testing.T) {
	rng := segRange(t)
	clean := TonePCM(48000, 2, 960, 11)
	if f := DetectClipping(clean, 1, 3, rng); f != nil {
		t.Fatalf("clean tone flagged clipping: %v", f)
	}
	clip := ClippedPCM(2, 960, 100, 8)
	f := DetectClipping(clip, 2, 3, rng)
	if f == nil || f.Code != media.CodeClipping || f.TrackID != 2 {
		t.Fatalf("want clipping fault with track, got %+v", f)
	}
}
