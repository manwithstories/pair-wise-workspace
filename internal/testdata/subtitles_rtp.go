package testdata

import (
	"fmt"
	"strings"

	"mediasync/internal/rtprepair"
)

// TimecodeList renders newline-separated labels from a fixture.
func TimecodeList(fx TimecodeFixture) []byte {
	return []byte(strings.Join(fx.Labels, "\n") + "\n")
}

// SubtitleKind selects a subtitle fixture variant.
type SubtitleKind string

const (
	SubtitleClean      SubtitleKind = "clean"
	SubtitleOverlap    SubtitleKind = "overlap"
	SubtitleBadTime    SubtitleKind = "bad_timestamp"
	SubtitleTruncated  SubtitleKind = "truncated_line"
	SubtitleWebVTT     SubtitleKind = "webvtt_clean"
	SubtitleVTTBadTime SubtitleKind = "webvtt_bad_timestamp"
)

// GenerateSRT renders a SubRip fixture. Damaged variants keep the surrounding
// records well-formed so recovery can be demonstrated.
func GenerateSRT(kind SubtitleKind, seed int64) []byte {
	var b strings.Builder
	cues := []struct {
		start, end, text string
	}{
		{"00:00:01,000", "00:00:02,500", "摄像机一：开场掌声"},
		{"00:00:02,500", "00:00:04,000", "主持人上台"},
		{"00:00:04,200", "00:00:06,000", "乐队起奏 <i>第一乐章</i>"},
		{"00:00:06,000", "00:00:08,000", "镜头切换至全景"},
		{"00:00:08,100", "00:00:09,500", "字幕组测试文本"},
	}
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, c.start, c.end, c.text)
	}
	s := b.String()
	switch kind {
	case SubtitleOverlap:
		// Third cue starts before the second ends.
		s = strings.Replace(s, "00:00:04,200 --> 00:00:06,000", "00:00:03,100 --> 00:00:06,000", 1)
	case SubtitleBadTime:
		s = strings.Replace(s, "00:00:04,200 --> 00:00:06,000", "00:00:04,200 -> 00:00:06,000", 1)
		s = strings.Replace(s, "00:00:06,000 --> 00:00:08,000", "00:00:06,0x0 --> 00:00:08,000", 1)
	case SubtitleTruncated:
		s = strings.Replace(s, "00:00:08,100 --> 00:00:09,500\n字幕组测试文本\n", "00:00:08,100 -->\n", 1)
	case SubtitleWebVTT, SubtitleVTTBadTime:
		return generateVTT(kind)
	default:
		_ = seed
	}
	return []byte(s)
}

func generateVTT(kind SubtitleKind) []byte {
	var b strings.Builder
	b.WriteString("WEBVTT - mediasync fixture\n\n")
	b.WriteString("NOTE 本文件由测试夹具生成\n\n")
	lines := []struct{ id, start, end, text string }{
		{"cue-1", "00:00:01.000", "00:00:02.500", "摄像机一：开场掌声"},
		{"cue-2", "00:00:02.500", "00:00:04.000", "主持人上台"},
		{"cue-3", "00:00:04.200", "00:00:06.000", "乐队起奏 <i>第一乐章</i>"},
		{"cue-4", "00:00:06.000", "00:00:08.000", "镜头切换至全景"},
		{"cue-5", "00:00:08.100", "00:00:09.500", "字幕组测试文本"},
	}
	for _, c := range lines {
		start, end := c.start, c.end
		if kind == SubtitleVTTBadTime && c.id == "cue-3" {
			start = "00:00:04,200"
		}
		fmt.Fprintf(&b, "%s\n%s --> %s align:start position:10%%\n%s\n\n", c.id, start, end, c.text)
	}
	return []byte(b.String())
}

// RTPPlan describes a generated packet stream.
type RTPPlan struct {
	Packets     int
	PayloadSize int
	SSRC        uint32
	StartSeq    uint16
	Timestamp   uint32
	// DuplicateEvery 0 disables duplication; otherwise every Nth packet is
	// retransmitted once, after a small delay.
	DuplicateEvery int
	// ReorderEvery 0 disables reordering; otherwise each Nth pair is swapped.
	ReorderEvery int
	// BurstLoss is the number of consecutive packets dropped per burst.
	BurstLoss int
	// BurstCount is how many bursts are injected.
	BurstCount int
	// BurstAt lists explicit packet indices (0-based) where bursts begin.
	// When empty, bursts are spread evenly across the stream.
	BurstAt []int
}

// RTPFixture is the generated datagrams plus the ground-truth bookkeeping the
// tests assert against.
type RTPFixture struct {
	Packets    [][]byte
	Offsets    []int64
	Duplicates int
	Reordered  int
	BurstLost  int
	Dropped    []int
}

// GenerateRTP produces a realistic busted capture: packets reordered in
// pairs, selectively duplicated, dropped in bursts, with sequence numbers that
// wrap exactly once.
func GenerateRTP(plan RTPPlan) RTPFixture {
	var fx RTPFixture
	if plan.PayloadSize <= 0 {
		plan.PayloadSize = 96
	}
	if plan.Packets <= 0 {
		plan.Packets = 1
	}
	seq := plan.StartSeq
	damage := map[int]bool{}
	if plan.BurstCount > 0 && plan.BurstLoss > 0 {
		step := plan.Packets / plan.BurstCount
		if step < plan.BurstLoss+1 {
			step = plan.BurstLoss + 1
		}
		at := plan.BurstAt
		if len(at) == 0 {
			for i := 0; i < plan.BurstCount; i++ {
				at = append(at, 10+i*step)
			}
		}
		for _, start := range at {
			for k := 0; k < plan.BurstLoss; k++ {
				idx := start + k
				if idx < plan.Packets {
					damage[idx] = true
					fx.Dropped = append(fx.Dropped, idx)
					fx.BurstLost++
				}
			}
		}
	}
	emit := func(idx int) {
		s := seq + uint16(idx)
		body := payload(int64(plan.SSRC), idx, plan.PayloadSize)
		fx.Offsets = append(fx.Offsets, int64(len(fx.Packets)))
		fx.Packets = append(fx.Packets, rtprepair.Encode(s, plan.Timestamp+uint32(idx)*3000, plan.SSRC, body))
	}
	for i := 0; i < plan.Packets; i++ {
		if damage[i] {
			continue
		}
		if plan.ReorderEvery > 0 && i%plan.ReorderEvery == 1 && i+1 < plan.Packets && !damage[i+1] {
			emit(i + 1)
			emit(i)
			fx.Reordered++
			i++
			continue
		}
		emit(i)
		if plan.DuplicateEvery > 0 && i%plan.DuplicateEvery == 0 {
			emit(i)
			fx.Duplicates++
		}
	}
	return fx
}
