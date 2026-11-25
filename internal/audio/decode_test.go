package audio

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"medpipe/internal/clock"
)

func ffmpegPresent() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// renderClip builds an MP4 whose audio is loud for the first half and silent
// for the second half.
func renderClip(t *testing.T, dir, name string, channels, rate int) string {
	t.Helper()
	if !ffmpegPresent() {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, name)
	// 4 s: 2 s sine then 2 s anullsrc, both at the requested layout/rate.
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-f", "lavfi", "-i", "anullsrc=r="+itoa(rate)+":cl=stereo:duration=2",
		"-filter_complex",
		"[0:a]aformat=channel_layouts=stereo:sample_rates="+itoa(rate)+"[a];"+
			"[1:a][a]concat=n=2:v=0:a=1[out]",
		"-map", "[out]",
		"-ac", itoa(channels), "-ar", itoa(rate),
		"-c:a", "aac", "-f", "mp4", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg render failed: %v\n%s", err, b)
	}
	return out
}

func TestAnalyzeSilenceStereo48(t *testing.T) {
	dir := t.TempDir()
	path := renderClip(t, dir, "s48.mp4", 2, 48000)
	det, err := AnalyzeSilence(context.Background(), path, DecodeOptions{
		StreamIndex: 0, Channels: 2, SourceRate: 48000, TargetRate: 48000,
	})
	if err != nil {
		t.Fatal(err)
	}
	ms := det.TotalSilenceMillis()
	if ms < 1500 || ms > 2300 {
		t.Fatalf("silence = %d ms, want roughly 2000 ms", ms)
	}
	if ms < 0 {
		t.Fatalf("negative silence: %d", ms)
	}
}

func TestAnalyzeSilenceResample441(t *testing.T) {
	dir := t.TempDir()
	path := renderClip(t, dir, "s441.mp4", 1, 44100)
	det, err := AnalyzeSilence(context.Background(), path, DecodeOptions{
		StreamIndex: 0, Channels: 1, SourceRate: 44100, TargetRate: CanonicalRate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ms := det.TotalSilenceMillis(); ms < 1500 || ms > 2300 {
		t.Fatalf("resampled silence = %d ms, want ~2000", ms)
	}
	// Detector must operate at the canonical 48 kHz rate.
	if det.cfg.SampleRate != CanonicalRate {
		t.Fatalf("detector rate = %d, want %d", det.cfg.SampleRate, CanonicalRate)
	}
}

func TestUnsupportedRateStopsTrack(t *testing.T) {
	dir := t.TempDir()
	path := renderClip(t, dir, "s.mp4", 2, 48000)
	_, err := AnalyzeSilence(context.Background(), path, DecodeOptions{
		StreamIndex: 0, Channels: 2, SourceRate: 22050,
	})
	if !clock.IsKind(err, clock.KindUnsupported) {
		t.Fatalf("want unsupported error, got %v", err)
	}
}

func TestUnsupportedChannelLayout(t *testing.T) {
	_, err := channelLayout(3)
	if !clock.IsKind(err, clock.KindUnsupported) {
		t.Fatalf("want unsupported layout, got %v", err)
	}
	for _, ch := range []int{1, 2, 6, 8} {
		if _, err := channelLayout(ch); err != nil {
			t.Fatalf("layout %d must map: %v", ch, err)
		}
	}
}

func TestDecodeCancellation(t *testing.T) {
	dir := t.TempDir()
	path := renderClip(t, dir, "long.mp4", 2, 48000)
	// Pre-cancel so the decoder is guaranteed to observe a canceled context,
	// avoiding a race with how fast the local pipe drains.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := AnalyzeSilence(ctx, path, DecodeOptions{
		StreamIndex: 0, Channels: 2, SourceRate: 48000,
	})
	if err == nil {
		t.Fatal("canceled decode returned no error")
	}
	if !clock.IsKind(err, clock.KindCanceled) {
		t.Fatalf("want canceled error, got %v", err)
	}
}

func TestDecodeStreamBoundedBlocks(t *testing.T) {
	dir := t.TempDir()
	path := renderClip(t, dir, "s.mp4", 2, 48000)
	var maxBlock int
	var blocks int
	err := DecodeStream(context.Background(), path,
		DecodeOptions{StreamIndex: 0, Channels: 2, SourceRate: 48000},
		func(block []int16) error {
			if len(block) > maxBlock {
				maxBlock = len(block)
			}
			if len(block) > blockSamples {
				t.Fatalf("block %d exceeds bound %d", len(block), blockSamples)
			}
			blocks++
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if blocks < 2 || maxBlock == 0 {
		t.Fatalf("streaming produced blocks=%d max=%d", blocks, maxBlock)
	}
}
