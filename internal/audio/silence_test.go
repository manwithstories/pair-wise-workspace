package audio_test

import (
	"testing"

	"mediasync/internal/audio"
	"mediasync/internal/media"
	"mediasync/internal/testdata"
)

func detectDefault(samples []int16) *audio.Result {
	return audio.Detect(samples, audio.DefaultFormat(), audio.DefaultOptions())
}

func TestSilenceMergesAcrossChunkBoundary(t *testing.T) {
	samples := testdata.GeneratePCM(testdata.DefaultPCMPlan())
	res := detectDefault(samples)
	if len(res.Runs) != 2 {
		t.Fatalf("got %d silence runs, want 2: %+v", len(res.Runs), res.Runs)
	}
	first := res.Runs[0]
	// 1s tone, then 1s silence, a 50ms blip, then 1s silence: the blip is
	// shorter than the 120ms split gap, so the run stays merged.
	if first.StartSample != 48000 {
		t.Errorf("first run starts at %d, want 48000", first.StartSample)
	}
	if first.EndSample != 146400 {
		t.Errorf("first run ends at %d, want 146400", first.EndSample)
	}
	if first.Merged < 2 {
		t.Errorf("run spans only %d chunks; boundary merge not exercised", first.Merged)
	}
	if res.SplitGaps != 1 {
		t.Errorf("split gaps = %d, want 1 (the 1s loud stretch)", res.SplitGaps)
	}
	if second := res.Runs[1]; second.EndSample != int64(len(samples)) {
		t.Errorf("tail run ends at %d, want %d", second.EndSample, len(samples))
	}
}

func TestLongGapSplitsSilence(t *testing.T) {
	// Silent 1s, loud 400ms (> default 120ms), silent 1s.
	plan := testdata.PCMPlan{SampleRate: 48000, Channels: 1, Segments: []testdata.PCMSegment{
		{Kind: "silent", Duration: 1000},
		{Kind: "tone", Duration: 400, ToneHz: 440},
		{Kind: "silent", Duration: 1000},
	}}
	res := detectDefault(testdata.GeneratePCM(plan))
	if len(res.Runs) != 2 {
		t.Fatalf("got %d runs, want 2: %+v", len(res.Runs), res.Runs)
	}
	if res.SplitGaps != 1 {
		t.Errorf("split gaps = %d, want 1", res.SplitGaps)
	}
	if res.Runs[0].EndSample > 48000+2400 {
		t.Errorf("first run ends at %d, should stop before the loud stretch", res.Runs[0].EndSample)
	}
	if res.Runs[1].StartSample < 48000+400*48-2400 {
		t.Errorf("second run starts at %d, inside the loud stretch", res.Runs[1].StartSample)
	}
}

func TestShortSilenceNotReported(t *testing.T) {
	plan := testdata.PCMPlan{SampleRate: 48000, Channels: 1, Segments: []testdata.PCMSegment{
		{Kind: "tone", Duration: 1000, ToneHz: 440},
		{Kind: "silent", Duration: 100}, // below the 300ms minimum
		{Kind: "tone", Duration: 1000, ToneHz: 440},
	}}
	res := detectDefault(testdata.GeneratePCM(plan))
	if len(res.Runs) != 0 {
		t.Fatalf("100ms silence reported as a run: %+v", res.Runs)
	}
}

func TestStereoAndSampleRates(t *testing.T) {
	for _, rate := range []int{8000, 16000, 44100, 48000} {
		plan := testdata.PCMPlan{SampleRate: rate, Channels: 2, Segments: []testdata.PCMSegment{
			{Kind: "tone", Duration: 500, ToneHz: 300},
			{Kind: "silent", Duration: 600},
		}}
		samples := testdata.GeneratePCM(plan)
		res := audio.Detect(samples, audio.Format{SampleRate: rate, Channels: 2}, audio.DefaultOptions())
		if len(res.Runs) != 1 {
			t.Fatalf("%dHz stereo: got %d runs", rate, len(res.Runs))
		}
		run := res.Runs[0]
		if got := run.DurationMS(); got < 550 || got > 650 {
			t.Errorf("%dHz: silence run is %dms, want ~600ms", rate, got)
		}
		if run.EndNS != res.Runs[0].EndNS || run.StartNS >= run.EndNS {
			t.Errorf("%dHz: bad run bounds %+v", rate, run)
		}
	}
}

func TestSilenceRatios(t *testing.T) {
	plan := testdata.PCMPlan{SampleRate: 48000, Channels: 1, Segments: []testdata.PCMSegment{
		{Kind: "tone", Duration: 1000, ToneHz: 200},
		{Kind: "silent", Duration: 1000},
	}}
	res := detectDefault(testdata.GeneratePCM(plan))
	if res.Frames != 96000 {
		t.Errorf("frames = %d, want 96000", res.Frames)
	}
	if ratio := res.SilentRatio(); ratio < 0.49 || ratio > 0.51 {
		t.Errorf("silent ratio = %.3f, want ~0.5", ratio)
	}
}

func TestEmptyAndLimits(t *testing.T) {
	res := detectDefault(nil)
	if res.Frames != 0 || len(res.Runs) != 0 {
		t.Errorf("empty input produced %+v", res)
	}
	bad := audio.Detect(nil, audio.Format{SampleRate: 0, Channels: 1}, audio.DefaultOptions())
	if len(bad.Issues) == 0 || bad.Issues[0].Stage != media.StageAudio {
		t.Errorf("invalid format not reported: %+v", bad.Issues)
	}
	tooMany := make([]int16, audio.MaxSamples+2)
	limited := audio.Detect(tooMany, audio.Format{SampleRate: 48000, Channels: 1}, audio.DefaultOptions())
	if limited.Frames != audio.MaxSamples {
		t.Errorf("frames = %d, want clamp to %d", limited.Frames, audio.MaxSamples)
	}
	found := false
	for _, is := range limited.Issues {
		if is.Code == media.CodeLimitExceeded {
			found = true
		}
	}
	if !found {
		t.Error("frame limit not reported")
	}
}

func TestPCMEncodingRoundtrip(t *testing.T) {
	original := testdata.GeneratePCM(testdata.DefaultPCMPlan())
	encoded := testdata.EncodePCM16LE(original)
	decoded := testdata.DecodePCM16LE(encoded)
	if len(decoded) != len(original) {
		t.Fatalf("decoded %d samples, want %d", len(decoded), len(original))
	}
	for i := range original {
		if decoded[i] != original[i] {
			t.Fatalf("sample %d = %d, want %d", i, decoded[i], original[i])
		}
	}
	// The encoded form must be what the detector would see from disk.
	a := audio.Detect(original, audio.DefaultFormat(), audio.DefaultOptions())
	b := audio.Detect(decoded, audio.DefaultFormat(), audio.DefaultOptions())
	if len(a.Runs) != len(b.Runs) {
		t.Fatalf("run count changed after PCM roundtrip: %d vs %d", len(a.Runs), len(b.Runs))
	}
}
