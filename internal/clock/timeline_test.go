package clock

import "testing"

func TestUnwrapPCR(t *testing.T) {
	// 从接近回绕点处正向小步长前进，真实穿越 2^33 边界。
	want := PCRModulus - 1500
	prev := Unwrap(0, want%PCRModulus, PCRModulus, false)
	if prev != want {
		t.Fatalf("first sample = %d, want %d", prev, want)
	}
	step := Tick(1000)
	for i := 0; i < 4; i++ {
		want += step
		got := Unwrap(prev, want%PCRModulus, PCRModulus, true)
		if got != want {
			t.Fatalf("iter %d: unwrap=%d want=%d (raw=%d)", i, got, want, want%PCRModulus)
		}
		prev = got
	}
	// 小幅度抖动应保持在同一周期，不应误判回绕。
	base := Tick(5_000_000)
	got := Unwrap(base, base+3, PCRModulus, true)
	if got != base+3 {
		t.Fatalf("jitter unwrap=%d want=%d", got, base+3)
	}
	got = Unwrap(base, base-3, PCRModulus, true)
	if got != base-3 {
		t.Fatalf("back jitter unwrap=%d want=%d", got, base-3)
	}
}

func TestUnwrapPTSFirstSample(t *testing.T) {
	if got := Unwrap(0, 12345, PTSModulus, false); got != 12345 {
		t.Fatalf("first sample = %d, want raw 12345", got)
	}
}

func TestDriftEstimator(t *testing.T) {
	var d DriftEstimator
	// 远端时钟每 1,000,000 本地 tick 走 1,001,000，即 +1000 ppm。
	for i := 0; i < 10; i++ {
		local := Tick(i) * 1_000_000
		remote := Tick(i) * 1_001_000
		d.Sample(local, remote)
	}
	if d.Samples() != 10 {
		t.Fatalf("samples=%d", d.Samples())
	}
	if d.PPM() < 999 || d.PPM() > 1001 {
		t.Fatalf("ppm=%.3f, want ~1000", d.PPM())
	}
	if d.Limited() {
		t.Fatal("normal drift must not be clamped")
	}
}

func TestDriftClamp(t *testing.T) {
	var d DriftEstimator
	d.Sample(0, 0)
	d.Sample(1_000_000, 2_000_000) // +100% 漂移，远超上限
	if !d.Limited() {
		t.Fatal("extreme drift must be clamped")
	}
	if d.PPM() != MaxDriftPPM {
		t.Fatalf("ppm=%.1f, want clamp %d", d.PPM(), MaxDriftPPM)
	}
}

func TestTimelineMonotonicAndVFR(t *testing.T) {
	tl := NewTimeline()
	tr := tl.EnsureTrack(0x0100)
	gaps := []Tick{900_000, 450_000, 1_800_000} // 30fps/60fps/15fps 混合 VFR
	raw := Tick(0)
	tr.Append(TimelineEvent{Index: 0, At: raw, RawPTS: raw, Kind: FrameKey})
	for i, g := range gaps {
		raw += g
		ev := tr.Append(TimelineEvent{Index: i + 1, At: raw, RawPTS: raw, Kind: FrameInter})
		if ev.VFRGap != g {
			t.Fatalf("VFR gap %d = %d, want %d", i, ev.VFRGap, g)
		}
	}
	if !tl.Monotonic() {
		t.Fatal("timeline must be monotonic")
	}
	// 倒退时间戳必须被修正为严格递增。
	bad := tr.Append(TimelineEvent{Index: 99, At: raw - 5, RawPTS: raw - 5})
	if bad.At <= raw {
		t.Fatalf("regressed timestamp not fixed: %d <= %d", bad.At, raw)
	}
}

func TestUntrustRecoversOnKeyframe(t *testing.T) {
	tl := NewTimeline()
	tr := tl.EnsureTrack(0x0200)
	tr.MarkUntrust()
	if !tr.Untrust {
		t.Fatal("mark untrust failed")
	}
	tr.Append(TimelineEvent{Index: 1, At: 27_000, RawPTS: 27_000, Kind: FrameInter})
	if !tr.Untrust {
		t.Fatal("inter frame must not clear untrust")
	}
	tr.Append(TimelineEvent{Index: 2, At: 54_000, RawPTS: 54_000, Kind: FrameKey})
	if tr.Untrust {
		t.Fatal("keyframe must restore trust")
	}
	kf := tr.Keyframes()
	if len(kf) != 1 || tr.Events[kf[0]].Index != 2 {
		t.Fatalf("keyframes=%v", kf)
	}
}

func TestFlattenOrdering(t *testing.T) {
	tl := NewTimeline()
	a := tl.EnsureTrack(0x0100)
	b := tl.EnsureTrack(0x0101)
	a.Append(TimelineEvent{Index: 0, At: 100, RawPTS: 100})
	b.Append(TimelineEvent{Index: 0, At: 50, RawPTS: 50})
	a.Append(TimelineEvent{Index: 1, At: 200, RawPTS: 200})
	flat := tl.Flatten()
	if len(flat) != 3 || flat[0].At != 50 || flat[1].At != 100 || flat[2].At != 200 {
		t.Fatalf("flatten order wrong: %+v", flat)
	}
	if ids := tl.TrackIDs(); ids[0] != 0x0100 || ids[1] != 0x0101 {
		t.Fatalf("track ids not sorted: %v", ids)
	}
}
