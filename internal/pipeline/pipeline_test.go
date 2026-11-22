package pipeline

import (
	"bytes"
	"math"
	"testing"
)

func TestLevelDbFS(t *testing.T) {
	silent := PCMFrame{Samples: []int16{0, 0, 0}, Channels: 1}
	if !math.IsInf(LevelDbFS(silent), -1) {
		t.Fatalf("zero PCM must be -Inf, got %v", LevelDbFS(silent))
	}
	loud := PCMFrame{Samples: []int16{32767, -32767}, Channels: 1}
	if LevelDbFS(loud) <= -1 {
		t.Fatalf("full scale must be near 0 dBFS, got %v", LevelDbFS(loud))
	}
}

func TestDetectSilencePCM(t *testing.T) {
	cfg := SilenceConfig{ThresholdDb: -50, MinMergeMs: 50, MaxGapMs: 100}
	frames := []PCMFrame{
		{StartNs: 0, DurationNs: 200_000_000, Samples: []int16{0, 0}, Channels: 1}, // 静音
		{StartNs: 200_000_000, DurationNs: 200_000_000, Samples: []int16{30000}, Channels: 1}, // 有声
		{StartNs: 400_000_000, DurationNs: 100_000_000, Samples: []int16{0}, Channels: 1}, // 短静音(合并)
	}
	ivs := DetectSilencePCM(frames, cfg)
	var silentMs int64
	for _, iv := range ivs {
		if iv.Kind == SilenceKnown {
			silentMs += iv.Ms
		}
	}
	if silentMs < 200 {
		t.Fatalf("expected >=200ms silence, got %d (%+v)", silentMs, ivs)
	}
	if TotalSilenceMs(ivs) != silentMs {
		t.Fatal("TotalSilenceMs mismatch")
	}
}

func TestUnknownGapMarked(t *testing.T) {
	cfg := SilenceConfig{ThresholdDb: -50, MinMergeMs: 50, MaxGapMs: 50}
	frames := []PCMFrame{
		{StartNs: 0, DurationNs: 100_000_000, Samples: []int16{100}, Channels: 1},
		// 缺口 300ms > MaxGapMs。
		{StartNs: 400_000_000, DurationNs: 100_000_000, Samples: []int16{100}, Channels: 1},
	}
	ivs := DetectSilencePCM(frames, cfg)
	found := false
	for _, iv := range ivs {
		if iv.Kind == SilenceUnknown {
			found = true
			if iv.Ms != 300 {
				t.Fatalf("gap ms = %d want 300", iv.Ms)
			}
		}
	}
	if !found {
		t.Fatalf("missing-sample gap must be marked unknown: %+v", ivs)
	}
}

func TestCompressedStatsUnknown(t *testing.T) {
	cfg := DefaultSilenceConfig()
	stats := []CompressedStat{
		{StartNs: 0, EndNs: 100_000_000, Silent: true, Available: true},
		{StartNs: 100_000_000, EndNs: 200_000_000, Available: false},
	}
	ivs := DetectSilenceStats(stats, cfg)
	kinds := map[string]bool{}
	for _, iv := range ivs {
		kinds[iv.Kind] = true
	}
	if !kinds[SilenceKnown] || !kinds[SilenceUnknown] {
		t.Fatalf("expected both silent and unknown, got %+v", ivs)
	}
}

func TestFingerprintDeterministicAndDedup(t *testing.T) {
	data := []byte("aligned-segment-bytes-0123456789")
	r := bytes.NewReader(data)
	fp := NewFingerprinter(func(off int64, p []byte) (int, error) {
		return r.ReadAt(p, off)
	})
	ranges := []ByteRange{{Offset: 0, Length: 10}, {Offset: 10, Length: 8}}
	h1, _, err := fp.HashRanges(ranges)
	if err != nil {
		t.Fatal(err)
	}
	h2, _, err := fp.HashRanges(ranges)
	if err != nil || h1 != h2 {
		t.Fatal("fingerprint must be deterministic across runs")
	}
	if len(h1) != 64 {
		t.Fatalf("sha256 hex must be 64 chars, got %d", len(h1))
	}
	// 不同布局不得碰撞。
	h3, _, _ := fp.HashRanges([]ByteRange{{Offset: 0, Length: 18}})
	if h3 == h1 {
		t.Fatal("layout boundaries must participate in hash")
	}

	segs := []*Segment{
		{ID: "s1", SHA256: h1},
		{ID: "s2", SHA256: h1}, // 与 s1 字节一致
		{ID: "s3", SHA256: h3},
	}
	if n := Dedup(segs); n != 1 {
		t.Fatalf("dedup count = %d want 1", n)
	}
	if segs[1].DuplicateOf != "s1" || segs[0].DuplicateOf != "" {
		t.Fatalf("duplicate marking wrong: %+v", segs)
	}
}

func TestFingerprintRerunIdentical(t *testing.T) {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i * 7)
	}
	fp1 := NewFingerprinter(func(off int64, p []byte) (int, error) {
		return bytes.NewReader(data).ReadAt(p, off)
	})
	fp2 := NewFingerprinter(func(off int64, p []byte) (int, error) {
		return bytes.NewReader(data).ReadAt(p, off)
	})
	rg := []ByteRange{{Offset: 100, Length: 500}, {Offset: 2000, Length: 900}}
	a, _, _ := fp1.HashRanges(rg)
	b, _, _ := fp2.HashRanges(rg)
	if a != b {
		t.Fatal("independent reruns must yield identical hashes")
	}
}
