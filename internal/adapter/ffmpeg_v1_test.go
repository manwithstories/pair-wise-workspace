package adapter

import (
	"context"
	"path/filepath"
	"testing"

	"avcore/internal/clock"
)

func TestFFmpegV1Encode(t *testing.T) {
	dir := t.TempDir()
	sample := makeSample(t, dir, "30", 4)
	enc, err := NewFFmpegV1("")
	if err != nil {
		t.Skipf("ffmpeg unavailable: %v", err)
	}
	out := filepath.Join(dir, "seg0.m4s")
	req := EncodeRequest{
		Input: sample, StartTicks: 0, DurationTicks: clock.TicksPerSecond,
		TargetBPS: 900_000, Width: 320, Height: 240, FrameRate: clock.Rate30, Output: out,
	}
	res, err := enc.Encode(context.Background(), req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if res.Bytes <= 0 || res.ActualBPS <= 0 {
		t.Errorf("empty/implausible result: %+v", res)
	}
	// Re-encoding the identical request to a fresh path must reproduce a
	// byte-identical segment (x264 deterministic preset, no container time jitter).
	out2 := filepath.Join(dir, "seg0b.m4s")
	req2 := req
	req2.Output = out2
	res2, err := enc.Encode(context.Background(), req2)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if res.Bytes != res2.Bytes {
		t.Errorf("deterministic encode size drift: %d vs %d", res.Bytes, res2.Bytes)
	}
}

func TestFFmpegV1Measure(t *testing.T) {
	dir := t.TempDir()
	sample := makeSample(t, dir, "30", 3)
	enc, err := NewFFmpegV1("")
	if err != nil {
		t.Skipf("ffmpeg unavailable: %v", err)
	}
	out := filepath.Join(dir, "seg.m4s")
	req := EncodeRequest{
		Input: sample, StartTicks: 0, DurationTicks: clock.TicksPerSecond,
		TargetBPS: 2_000_000, Width: 320, Height: 240, FrameRate: clock.Rate30, Output: out,
	}
	if _, err := enc.Encode(context.Background(), req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	m, err := enc.Measure(context.Background(), sample, req)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if m.PSNRMeanDB < 28 {
		t.Errorf("PSNR mean %.2f below 28 dB", m.PSNRMeanDB)
	}
	if m.SSIMMean < 0.90 {
		t.Errorf("SSIM mean %.4f below 0.90", m.SSIMMean)
	}
}

func TestMetricParsing(t *testing.T) {
	log := "frame= 30\n[Parsed_ssim_2 @ x] SSIM Y:0.99 U:0.99 V:0.99 All:0.992287 (21.12)\n" +
		"[Parsed_psnr_3 @ x] PSNR y:42.0 u:41.4 v:41.7 average:41.862681 min:40.8 max:44.2\n"
	s, ok := extractSSIM(log)
	if !ok || s < 0.99 || s > 0.993 {
		t.Errorf("ssim parse = %v ok=%v", s, ok)
	}
	p, ok := extractPSNR(log)
	if !ok || p < 41.8 || p > 41.9 {
		t.Errorf("psnr parse = %v ok=%v", p, ok)
	}
	if _, ok := extractPSNR("garbage"); ok {
		t.Error("missing PSNR must report failure")
	}
}
