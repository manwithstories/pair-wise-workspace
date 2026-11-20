package subtitle

import (
	"strings"
	"testing"

	"mediagraph/internal/timeline"
)

const goodSRT = `1
00:00:01,000 --> 00:00:02,500
Hello world

2
00:00:03,000 --> 00:00:04,000
Second line
`

func TestParseGoodSRT(t *testing.T) {
	res := Parse([]byte(goodSRT))
	if res.Format != "srt" {
		t.Fatalf("format=%s", res.Format)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if len(res.Cues) != 2 {
		t.Fatalf("cues=%d", len(res.Cues))
	}
	if res.Cues[0].Start != timeline.MillisToTicks(1000) {
		t.Fatalf("start=%d", res.Cues[0].Start)
	}
	if res.Cues[0].ByteStart != 0 {
		t.Fatalf("byte start=%d", res.Cues[0].ByteStart)
	}
}

func TestParseWebVTT(t *testing.T) {
	vtt := "WEBVTT\n\n00:01.000 --> 00:02.000\nHi\n\n00:00:03.000 --> 00:00:04.500 align:start\nThere\n"
	res := Parse([]byte(vtt))
	if res.Format != "webvtt" {
		t.Fatalf("format=%s", res.Format)
	}
	if len(res.Errors) != 0 || len(res.Cues) != 2 {
		t.Fatalf("cues=%d errors=%v", len(res.Cues), res.Errors)
	}
	if res.Cues[0].Start != timeline.MillisToTicks(1000) {
		t.Fatalf("start=%d", res.Cues[0].Start)
	}
}

// 损坏样例：非法时间 → 截断块 → 重叠 → 后续可恢复字幕。
const corruptSRT = `1
00:00:01,000 --> 00:00:02,000
ok one

2
99:99:99,999 --> 00:00:03,000
bad time

3
00:00:03,000 --> 00:00:04,000

4
00:00:03,000 --> 00:00:04,000
ok four

5
00:00:03,500 --> 00:00:04,500
overlap

6
00:00:05,000 --> 00:00:06,000
recovered
`

func TestParseCorruptRecovers(t *testing.T) {
	res := Parse([]byte(corruptSRT))
	kinds := map[string]int{}
	var firstOffset int64 = -1
	for _, e := range res.Errors {
		kinds[e.Kind]++
		if firstOffset < 0 || e.Offset < firstOffset {
			firstOffset = e.Offset
		}
	}
	if kinds["invalid_time"] != 1 || kinds["truncated"] != 1 || kinds["overlap"] != 1 {
		t.Fatalf("kinds=%v", kinds)
	}
	// 首个错误必须定位到非法时间行的真实字节偏移
	badOff := int64(strings.Index(corruptSRT, "99:99:99"))
	if firstOffset != badOff {
		t.Fatalf("first offset=%d want %d", firstOffset, badOff)
	}
	// 恢复：最后一个有效字幕为 "recovered"
	last := res.Cues[len(res.Cues)-1]
	if last.Text != "recovered" {
		t.Fatalf("last cue=%q", last.Text)
	}
	// 重叠字幕保留
	if len(res.Cues) != 4 {
		t.Fatalf("cues=%d", len(res.Cues))
	}
}

func TestTruncatedAtEOF(t *testing.T) {
	res := Parse([]byte("1\n00:00:01,000 --> 00:00:02,000\n"))
	if len(res.Errors) != 1 || res.Errors[0].Kind != "truncated" {
		t.Fatalf("errors=%v", res.Errors)
	}
	if len(res.Cues) != 0 {
		t.Fatalf("cues=%d", len(res.Cues))
	}
}
