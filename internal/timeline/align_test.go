package timeline

import (
	"math"
	"testing"

	"mediad/internal/media"
)

func makeTrack(cam string, n int, driftPPM float64, vfr bool, jumpAt int, jumpTicks int64) *TrackSamples {
	tr := &TrackSamples{
		Key:       media.TrackKey{Camera: cam, Language: "und", Codec: "avc1", Role: "main"},
		Timescale: 90000,
	}
	ref := int64(0)
	for i := 0; i < n; i++ {
		dur := int64(3000)
		if vfr && i%3 == 0 {
			dur = 3753 // 8000/1001-style jitter
		}
		local := ref + int64(float64(ref)*driftPPM/1e6)
		if i == jumpAt {
			local += jumpTicks
		}
		tr.Samples = append(tr.Samples, media.Sample{
			Key:      tr.Key,
			DTS:      local,
			PTS:      local,
			Duration: dur,
			Sync:     true,
		})
		ref += dur
	}
	return tr
}

func TestAlignCorrectsDrift(t *testing.T) {
	refTrack := makeTrack("cam1", 200, 0, false, -1, 0)
	drifted := makeTrack("cam2", 200, 120, false, -1, 0)
	res, err := Align([]*TrackSamples{refTrack, drifted}, AlignOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Fits) != 2 {
		t.Fatalf("fits=%d", len(res.Fits))
	}
	// A camera drifting +120 ppm runs ahead of the reference, so the local->ref
	// model slope is slightly negative (~-120 ppm); correction removes it.
	if res.Fits[1].SlopePPM > -100 || res.Fits[1].SlopePPM < -140 {
		t.Fatalf("slope=%.2f ppm, want ~-120", res.Fits[1].SlopePPM)
	}
	if res.P95SyncMS > 20 {
		t.Fatalf("p95 sync=%.3f ms exceeds 20 ms", res.P95SyncMS)
	}
}

func TestAlignPreservesVFRDurations(t *testing.T) {
	refTrack := makeTrack("cam1", 120, 0, true, -1, 0)
	aligned := makeTrack("cam2", 120, 60, true, -1, 0)
	want := make([]int64, len(aligned.Samples))
	for i := range aligned.Samples {
		want[i] = aligned.Samples[i].Duration
	}
	res, err := Align([]*TrackSamples{refTrack, aligned}, AlignOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got []*TrackSamples
	for _, tr := range res.Tracks {
		if tr.Key.Camera == "cam2" {
			got = append(got, tr)
		}
	}
	if len(got) != 1 {
		t.Fatal("cam2 missing from output")
	}
	for i := range got[0].Samples {
		if got[0].Samples[i].Duration != want[i] {
			t.Fatalf("sample %d duration changed from %d to %d (VFR must be preserved)",
				i, want[i], got[0].Samples[i].Duration)
		}
	}
}

func TestAlignIsolatesJump(t *testing.T) {
	refTrack := makeTrack("cam1", 300, 0, false, -1, 0)
	// cam2 has a single gross 5 s jump plus normal 90 ppm drift.
	jumped := makeTrack("cam2", 300, 90, false, 150, 5*90000)
	clean := makeTrack("cam3", 300, 0, false, -1, 0)

	res, err := Align([]*TrackSamples{refTrack, jumped, clean}, AlignOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var cam3Fit, cam2Fit CameraFit
	for _, f := range res.Fits {
		switch f.Camera {
		case "cam2":
			cam2Fit = f
		case "cam3":
			cam3Fit = f
		}
	}
	if cam2Fit.Jumps == 0 {
		t.Fatalf("expected cam2's jump to be detected: %+v", cam2Fit)
	}
	// The clean camera must be unaffected: its fit stays near zero.
	if math.Abs(cam3Fit.SlopePPM) > 1 {
		t.Fatalf("cam3 contaminated by cam2 jump: slope=%.3f ppm", cam3Fit.SlopePPM)
	}
	if res.P95SyncMS > 20 {
		t.Fatalf("p95 sync=%.3f ms after correction", res.P95SyncMS)
	}
}

func TestAlignRejectsEmpty(t *testing.T) {
	if _, err := Align(nil, AlignOptions{}); err == nil {
		t.Fatal("expected error for empty track set")
	}
	if media.CodeOf(func() error { _, e := Align(nil, AlignOptions{}); return e }()) != media.CodeNoTrack {
		t.Fatal("expected ERR_NO_TRACK")
	}
}
