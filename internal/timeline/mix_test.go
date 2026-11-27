package timeline

import (
	"testing"

	"review/internal/clock"
)

func makeTrack(id string, offset, dur int64, n int, skip ...int) Track {
	tr := Track{ID: id}
	skipSet := map[int]bool{}
	for _, s := range skip {
		skipSet[s] = true
	}
	for i := 0; i < n; i++ {
		if skipSet[i] {
			continue
		}
		tr.Samples = append(tr.Samples, Sample{Tick: offset + int64(i)*dur, Dur: dur, Ref: i})
	}
	return tr
}

func TestMixSyncError(t *testing.T) {
	base, _ := clock.NewBase(90000, 48000) // 720000
	const frame = 24000                    // 1/30s
	// 三机位：0ms、8ms、15ms 偏移
	tracks := []Track{
		makeTrack("camA", 0, frame, 40),
		makeTrack("camB", 5760, frame, 40),   // 8ms
		makeTrack("camC", 10800, frame, 40),  // 15ms
	}
	tl := Mix(tracks, frame, base)
	if len(tl.Gaps) != 0 {
		t.Fatalf("unexpected gaps: %v", tl.Gaps)
	}
	if tl.SyncMaxMs < 14.9 || tl.SyncMaxMs > 15.1 {
		t.Fatalf("sync_max=%v, want ~15ms", tl.SyncMaxMs)
	}
	if tl.SyncMaxMs > 20 {
		t.Fatal("sync exceeds 20ms budget")
	}
	// 每槽三轨全命中
	for _, s := range tl.Slots[:40] {
		if len(s.Hits) != 3 {
			t.Fatalf("slot %d hits=%v", s.Tick, s.Hits)
		}
	}
}

func TestMixGapKeepsSource(t *testing.T) {
	base, _ := clock.NewBase(90000)
	const frame = 24000
	tracks := []Track{
		makeTrack("camA", 0, frame, 10),
		makeTrack("camB", 0, frame, 10, 4, 5), // 缺两帧
	}
	tl := Mix(tracks, frame, base)
	if len(tl.Gaps) != 2 {
		t.Fatalf("gaps=%v", tl.Gaps)
	}
	for _, g := range tl.Gaps {
		if g.TrackID != "camB" {
			t.Fatalf("gap source=%s, want camB", g.TrackID)
		}
	}
	if tl.Gaps[0].Tick != 4*frame {
		t.Fatalf("gap tick=%d", tl.Gaps[0].Tick)
	}
	ferrs := tl.GapFaults()
	if len(ferrs) != 2 || ferrs[0].Source != "camB" {
		t.Fatalf("faults=%v", ferrs)
	}
}

func TestMixAudioBlocks(t *testing.T) {
	base, _ := clock.NewBase(48000)
	const block = 1600 // 1/30s @48000：1600 样本
	tracks := []Track{
		makeTrack("camA/pcm", 0, block, 40, 20), // 缺一块 PCM
	}
	tl := Mix(tracks, block, base)
	if len(tl.Gaps) != 1 || tl.Gaps[0].TrackID != "camA/pcm" {
		t.Fatalf("gaps=%v", tl.Gaps)
	}
}

func TestNearestTolerance(t *testing.T) {
	s := []Sample{{Tick: 0}, {Tick: 100}, {Tick: 200}}
	if _, ok := nearest(s, 160, 30); ok {
		t.Fatal("160 is 40 away from 200, tol 30 should miss")
	}
	if got, ok := nearest(s, 195, 30); !ok || got.Tick != 200 {
		t.Fatalf("got %v ok=%v", got, ok)
	}
}
