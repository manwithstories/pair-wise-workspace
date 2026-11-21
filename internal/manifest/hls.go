// Package manifest 解析 HLS 媒体清单与 DASH MPD，产出带来源偏移的分片引用。
package manifest

import (
	"strconv"
	"strings"

	"mediadeliver/internal/clock"
	"mediadeliver/internal/fault"
)

// 解析上限，保证内存有界。
const (
	MaxSegments = 100000
	MaxLineLen  = 8192
)

// Segment 是一个分片引用，Offset 为其在清单源中的原始字节偏移。
type Segment struct {
	Index    int64          `json:"index"`
	Duration clock.Rational `json:"duration"`
	URI      string         `json:"uri"`
	Offset   int64          `json:"offset"`
}

// Playlist 是解析结果；截断清单保留已解析条目并以 Truncated 标记缺失尾部。
type Playlist struct {
	Source      string         `json:"source"`
	Segments    []Segment      `json:"segments"`
	Duration    clock.Rational `json:"duration"` // 全部分片时长之和
	Truncated   bool           `json:"truncated"`
	Faults      fault.List     `json:"faults,omitempty"`
}

// ParseHLS 解析 HLS 媒体清单（m3u8）。
func ParseHLS(source string, data []byte) Playlist {
	p := Playlist{Source: source}
	var seq int64 = 0
	var haveSeq bool
	var pendingDur clock.Rational
	var pendingOff int64 = -1
	var havePending bool

	lines := strings.Split(string(data), "\n")
	off := int64(0)
	idx := int64(0)
loop:
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		lineOff := off
		off += int64(len(raw)) + 1
		if len(line) > MaxLineLen {
			p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
				Offset: lineOff, Message: "line exceeds limit", Advice: "检查清单是否损坏"})
			p.Truncated = true
			break
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			v, err := strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
			if err != nil {
				p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
					Offset: lineOff, Message: "bad media sequence", Advice: "修正序号后重试"})
				continue
			}
			seq, haveSeq = v, true
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			// 仅校验可解析性
			if _, err := strconv.ParseFloat(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"), 64); err != nil {
				p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
					Offset: lineOff, Message: "bad target duration", Advice: "修正目标时长"})
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			d, err := strconv.ParseFloat(strings.TrimPrefix(strings.SplitN(line, ",", 2)[0], "#EXTINF:"), 64)
			if err != nil || d <= 0 {
				p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
					Offset: lineOff, Message: "bad EXTINF duration", Advice: "跳过该分片"})
				continue
			}
			pendingDur = clock.New(int64(d*1000000), 1000000)
			pendingOff, havePending = lineOff, true
		case line == "" || strings.HasPrefix(line, "#"):
			// 其他标签与空行忽略
		default:
			// URI 行
			if !havePending {
				p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
					Offset: lineOff, Message: "URI without EXTINF", Advice: "补齐时长标签"})
				continue
			}
			if len(p.Segments) >= MaxSegments {
				p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
					Offset: lineOff, Message: "segment limit exceeded", Advice: "拆分清单"})
				p.Truncated = true
				break loop
			}
			if !haveSeq {
				seq = 0
			}
			p.Segments = append(p.Segments, Segment{Index: seq + idx, Duration: pendingDur, URI: line, Offset: pendingOff})
			p.Duration = p.Duration.Add(pendingDur)
			idx++
			havePending = false
		}
	}
	// 截断检测：存在未配对的 EXTINF，或缺少 ENDLIST。
	if havePending || !strings.Contains(string(data), "#EXT-X-ENDLIST") {
		p.Truncated = true
		p.Faults = p.Faults.Add(fault.Fault{Stage: fault.StageManifest, Source: source,
			Offset: off, Recoverable: true, Message: "manifest truncated, tail missing",
			Advice: "保留已解析条目，等待尾部重传"})
	}
	return p
}
