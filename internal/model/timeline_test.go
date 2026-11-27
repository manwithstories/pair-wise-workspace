package model

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestNewClockRejectsZeroRate(t *testing.T) {
	if _, err := NewClock(0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("零采样率应返回 ErrInvalidInput，实际 %v", err)
	}
}

func TestSamplesToDuration(t *testing.T) {
	c, err := NewClock(48000)
	if err != nil {
		t.Fatalf("NewClock: %v", err)
	}
	if got := c.SamplesToDuration(48000); got != time.Second {
		t.Fatalf("48000 采样在 48kHz 下应为 1s，实际 %v", got)
	}
	if got := c.SamplesToDuration(24000); got != 500*time.Millisecond {
		t.Fatalf("24000 采样应为 500ms，实际 %v", got)
	}
}

func TestFrameBytesOverflow(t *testing.T) {
	c, _ := NewClock(48000)
	if _, err := c.FrameBytes(MaxSamples, 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("块对齐为 0 应报错，实际 %v", err)
	}
	if _, err := c.FrameBytes(1<<60, 1024); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("乘法溢出应报 ErrInvalidInput，实际 %v", err)
	}
	if n, err := c.FrameBytes(100, 4); err != nil || n != 400 {
		t.Fatalf("100 帧 * 4 字节应为 400，得到 %d, %v", n, err)
	}
}

func TestMul64Checked(t *testing.T) {
	if _, err := Mul64Checked(1<<62, 8); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("上溢未被检测: %v", err)
	}
	if r, err := Mul64Checked(-(1 << 62), 2); err != nil || r != math.MinInt64 {
		t.Fatalf("(-2^62)*2 应等于 MinInt64，得到 %d, %v", r, err)
	}
	if _, err := Mul64Checked(-(1<<62), 4); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("下溢未被检测: %v", err)
	}
	if _, err := Mul64Checked(math.MinInt64, 2); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("MinInt64 乘法溢出未被检测: %v", err)
	}
	if r, err := Mul64Checked(math.MinInt64, 1); err != nil || r != math.MinInt64 {
		t.Fatalf("MinInt64*1 应保持原值，得到 %d, %v", r, err)
	}
}

func TestGapAddAndMerge(t *testing.T) {
	c, _ := NewClock(48000)
	tl, err := NewTrackTimeline(1, c, 1000)
	if err != nil {
		t.Fatalf("NewTrackTimeline: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("AddGap: %v", err)
		}
	}
	must(tl.AddGap(Gap{Start: 100, End: 200, Kind: GapKindLoss}))
	must(tl.AddGap(Gap{Start: 150, End: 300, Kind: GapKindLoss})) // 重叠，合并
	must(tl.AddGap(Gap{Start: 400, End: 500, Kind: GapKindCRC}))  // 独立缺口
	if len(tl.Gaps) != 2 {
		t.Fatalf("合并后应剩 2 个缺口，实际 %d: %+v", len(tl.Gaps), tl.Gaps)
	}
	if tl.Gaps[0].Start != 100 || tl.Gaps[0].End != 300 {
		t.Fatalf("首个缺口合并结果错误: %+v", tl.Gaps[0])
	}
	if tl.GapSamples() != 300 {
		t.Fatalf("缺口采样总数应为 300，实际 %d", tl.GapSamples())
	}
	// 越界缺口必须拒绝。
	if err := tl.AddGap(Gap{Start: 900, End: 1001, Kind: GapKindLoss}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("越界缺口应被拒绝，实际 %v", err)
	}
	if err := tl.AddGap(Gap{Start: -1, End: 10, Kind: GapKindLoss}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("负起点缺口应被拒绝，实际 %v", err)
	}
}

func TestAnchorIntervalsNA(t *testing.T) {
	c, _ := NewClock(48000)
	tl, _ := NewTrackTimeline(1, c, 48000)
	if got := tl.AnchorIntervals(); got != nil {
		t.Fatalf("无锚点时应返回 nil（N/A），实际 %+v", got)
	}
	tl.AddAnchor(Anchor{Sample: 0})
	if got := tl.AnchorIntervals(); got != nil {
		t.Fatalf("单锚点时应返回 nil（N/A），实际 %+v", got)
	}
	tl.AddAnchor(Anchor{Sample: 24000})
	tl.AddAnchor(Anchor{Sample: 48000})
	iv := tl.AnchorIntervals()
	if len(iv) != 2 || !iv[0].Present {
		t.Fatalf("应得到 2 个有效间隔，实际 %+v", iv)
	}
	if iv[0].Samples != 24000 || iv[0].Ms != 500 {
		t.Fatalf("首间隔应为 24000 采样 / 500ms，实际 %+v", iv[0])
	}
	// 重复锚点去重。
	tl.AddAnchor(Anchor{Sample: 24000})
	if len(tl.Anchors) != 3 {
		t.Fatalf("重复锚点未去重，锚点数 %d", len(tl.Anchors))
	}
}

func TestCheckSampleCount(t *testing.T) {
	if err := CheckSampleCount(-1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("负采样数应报错: %v", err)
	}
	if err := CheckSampleCount(MaxSamples + 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("超上限应报错: %v", err)
	}
	if err := CheckSampleCount(0); err != nil {
		t.Fatalf("零采样应合法: %v", err)
	}
}

func TestExitCodeMapping(t *testing.T) {
	if ExitCode(nil) != 0 {
		t.Fatal("nil 错误退出码应为 0")
	}
	if ExitCode(ErrUnsupportedCodec) != 2 {
		t.Fatal("不支持编码应映射退出码 2")
	}
	if ExitCode(ErrUnrecoverable) != 3 {
		t.Fatal("不可恢复应映射退出码 3")
	}
}
