package model

import (
	"math"
	"testing"
)

func TestToUnifiedExactAndRoundtrip(t *testing.T) {
	cases := []struct {
		ts    uint32
		tick  uint64
		want  uint64
	}{
		{90_000, 90_000, 1_000_000},
		{48_000, 48_000, 1_000_000},
		{1000, 1, 1000},
		{44_100, 44_100, 1_000_000},
		{90_000, 0, 0},
	}
	for _, c := range cases {
		got, err := ToUnified(c.ts, c.tick)
		if err != nil {
			t.Fatalf("ToUnified(%d,%d): %v", c.ts, c.tick, err)
		}
		if got != c.want {
			t.Fatalf("ToUnified(%d,%d)=%d want %d", c.ts, c.tick, got, c.want)
		}
		back, err := FromUnified(c.ts, got)
		if err != nil {
			t.Fatalf("FromUnified(%d,%d): %v", c.ts, got, err)
		}
		if back != c.tick {
			t.Fatalf("roundtrip %d -> %d -> %d", c.tick, got, back)
		}
	}
}

func TestToUnifiedNoFloatDrift(t *testing.T) {
	// 逐样本换算并保留余数（整数进位）后累加，必须严格等于一次性换算。
	const n = 10000
	const delta = 3000
	want, err := ToUnified(90_000, uint64(n)*delta)
	if err != nil {
		t.Fatal(err)
	}
	var acc, rem uint64
	for i := 0; i < n; i++ {
		v, r := unifiedWithRemainder(90_000, uint64(delta))
		acc += v
		rem += r
		acc += rem / 90_000
		rem %= 90_000
	}
	if acc != want {
		t.Fatalf("余数进位累加 %d != 一次性 %d", acc, want)
	}
}

// unifiedWithRemainder 返回 tick 换算结果的整数部分与 remaindertick 形式的余数，
// 供调用方按 timescale 进位，演示无浮点的精确累加。
func unifiedWithRemainder(timescale uint32, tick uint64) (uint64, uint64) {
	return tick / uint64(timescale) * UnifiedTimescale, tick % uint64(timescale) * UnifiedTimescale
}

func TestToUnifiedOverflow(t *testing.T) {
	if _, err := ToUnified(0, 1); err == nil {
		t.Fatal("timescale=0 应报错")
	}
	if _, err := ToUnified(MaxTimescale+1, 1); err == nil {
		t.Fatal("超上限 timescale 应报错")
	}
	if _, err := ToUnified(90_000, math.MaxUint64); err == nil {
		t.Fatal("超大 tick 应溢出报错")
	}
}

func TestValidateRejectsOverlappingRanges(t *testing.T) {
	tl := &Timeline{
		Tracks: []Track{{ID: 1, Kind: TrackVideo, Timescale: 90_000}},
		Clips: []Clip{
			{ID: "a", TrackID: 1, FirstSample: 0, SampleCount: 10},
			{ID: "b", TrackID: 1, FirstSample: 5, SampleCount: 10},
		},
	}
	if err := tl.Validate(); err == nil {
		t.Fatal("重叠样本区间应被拒绝")
	}
}

func TestValidateRejectsUnknownReferences(t *testing.T) {
	tl := &Timeline{
		Tracks:      []Track{{ID: 1, Kind: TrackVideo, Timescale: 90_000}},
		Clips:       []Clip{{ID: "a", TrackID: 2}},
		Transitions: []Transition{{ID: "t", TrackID: 1, FromClipID: "a", ToClipID: "missing"}},
	}
	if err := tl.Validate(); err == nil {
		t.Fatal("未知引用应被拒绝")
	}
}

func TestRepairFlagString(t *testing.T) {
	f := RepairWrap | RepairClipJump
	if got := f.String(); got != "wrap|clip_jump" {
		t.Fatalf("String()=%q", got)
	}
}
