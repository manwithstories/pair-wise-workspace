package render

import (
	"fmt"
	"image"
	"image/color"
	"testing"

	"aircheck/internal/media"
)

func slot(track string, seq int64, start, dur float64) media.Slot {
	return media.Slot{Track: track, Seq: seq, StartMs: start, DurMs: dur, File: fmt.Sprintf("%s/%d.jpg", track, seq)}
}

func TestComposePriority(t *testing.T) {
	// camA 优先，覆盖 0-80ms；camB 全程
	a := TrackSlots{Track: "camA", Priority: 1, Slots: []media.Slot{slot("camA", 0, 0, 40), slot("camA", 1, 40, 40)}}
	b := TrackSlots{Track: "camB", Priority: 2, Slots: []media.Slot{slot("camB", 0, 0, 120)}}
	plan, err := Compose([]TrackSlots{b, a}, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(plan.Frames))
	}
	if plan.Frames[0].Track != "camA" || plan.Frames[1].Track != "camA" || plan.Frames[2].Track != "camB" {
		t.Fatalf("priority order wrong: %+v", plan.Frames)
	}
	if plan.AlignMaxMs != 0 {
		t.Fatalf("align = %v, want 0", plan.AlignMaxMs)
	}
}

func TestComposeGapAndBlack(t *testing.T) {
	// camA 中间 40ms 为 GAP，camB 兜底；无 camB 处应为黑帧
	gap := media.Slot{Track: "camA", Seq: 1, StartMs: 40, DurMs: 40, Gap: true}
	a := TrackSlots{Track: "camA", Priority: 1, Slots: []media.Slot{slot("camA", 0, 0, 40), gap, slot("camA", 2, 80, 40)}}
	b := TrackSlots{Track: "camB", Priority: 2, Slots: []media.Slot{slot("camB", 0, 40, 40)}}
	plan, err := Compose([]TrackSlots{a, b}, 25)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Frames[1].Track != "camB" {
		t.Fatalf("gap not covered by lower track: %+v", plan.Frames[1])
	}
	// 无兜底轨时 GAP 输出黑帧并计数
	plan2, err := Compose([]TrackSlots{a}, 25)
	if err != nil {
		t.Fatal(err)
	}
	if !plan2.Frames[1].Black || plan2.GapFrames != 1 {
		t.Fatalf("gap should yield black frame: %+v gapFrames=%d", plan2.Frames[1], plan2.GapFrames)
	}
}

func TestComposeBadIsolation(t *testing.T) {
	bad := slot("camA", 1, 40, 40)
	bad.Bad = true
	a := TrackSlots{Track: "camA", Priority: 1, Slots: []media.Slot{slot("camA", 0, 0, 40), bad, slot("camA", 2, 80, 40)}}
	b := TrackSlots{Track: "camB", Priority: 2, Slots: []media.Slot{slot("camB", 0, 0, 120)}}
	plan, err := Compose([]TrackSlots{a, b}, 25)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Frames[1].Track != "camB" {
		t.Fatalf("bad frame not isolated: %+v", plan.Frames[1])
	}
	if len(plan.BadRanges) != 1 || plan.BadRanges[0].Seq != 1 || plan.BadRanges[0].FromOut != 1 || plan.BadRanges[0].ToOut != 1 {
		t.Fatalf("bad ranges = %+v", plan.BadRanges)
	}
}

func TestComposeDeterministic(t *testing.T) {
	a := TrackSlots{Track: "camA", Priority: 1, Slots: []media.Slot{slot("camA", 0, 0, 40)}}
	b := TrackSlots{Track: "camB", Priority: 1, Slots: []media.Slot{slot("camB", 0, 0, 40)}}
	p1, _ := Compose([]TrackSlots{a, b}, 25)
	p2, _ := Compose([]TrackSlots{b, a}, 25)
	if p1.Frames[0].Track != p2.Frames[0].Track || p1.Frames[0].Track != "camA" {
		t.Fatalf("tie-break not deterministic: %s vs %s", p1.Frames[0].Track, p2.Frames[0].Track)
	}
}

func TestRenderBlackAndCache(t *testing.T) {
	// 覆盖 0-40 与 80-120，中间 40-80 无画面 → 黑帧
	a := TrackSlots{Track: "camA", Priority: 1, Slots: []media.Slot{slot("camA", 0, 0, 40), slot("camA", 2, 80, 40)}}
	plan, err := Compose([]TrackSlots{a}, 25)
	if err != nil {
		t.Fatal(err)
	}
	loads := 0
	frames, err := Render(plan, func(file string) (image.Image, error) {
		loads++
		return solid(16, 16, color.NRGBA{200, 0, 0, 255}), nil
	}, 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	if loads != 2 {
		t.Fatalf("loads = %d, want 2", loads)
	}
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	r, _, _, _ := frames[1].At(0, 0).RGBA()
	if r != 0 {
		t.Fatalf("middle frame not black")
	}
}
