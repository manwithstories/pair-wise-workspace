// Package subtitle 解析 SRT/WebVTT 字幕，逐条产出可用记录；
// 非法时间、重叠区间、截断行均报告源字节偏移，并继续处理后续可恢复字幕。
package subtitle

import (
	"fmt"
	"strings"

	"mediagraph/internal/timeline"
)

// Cue 为一条可用字幕记录，时间为主基准刻度。
type Cue struct {
	Index     int
	Start     int64
	End       int64
	Text      string
	ByteStart int64 // 块首字节偏移（含）
	ByteEnd   int64 // 块尾字节偏移（不含）
}

// ParseError 为可恢复解析错误，携带源字节偏移。
type ParseError struct {
	Offset int64
	Kind   string // invalid_time / overlap / truncated
	Detail string
}

func (e ParseError) Error() string {
	return fmt.Sprintf("%s at byte %d: %s", e.Kind, e.Offset, e.Detail)
}

// Result 汇总解析产物：可用字幕与全部可恢复错误。
type Result struct {
	Format string
	Cues   []Cue
	Errors []ParseError
}

// line 为带字节偏移的物理行。
type line struct {
	text   string
	offset int64
}

// splitLines 按行切分并记录每行起始字节偏移，兼容 \n 与 \r\n。
func splitLines(data []byte) []line {
	var out []line
	off := int64(0)
	for off < int64(len(data)) {
		end := off
		for end < int64(len(data)) && data[end] != '\n' {
			end++
		}
		text := strings.TrimRight(string(data[off:end]), "\r")
		out = append(out, line{text: text, offset: off})
		off = end + 1
	}
	return out
}

// Parse 自动识别格式（WEBVTT 头 → WebVTT，否则 SRT）。
func Parse(data []byte) Result {
	trimmed := strings.TrimLeft(string(data), "\uFEFF \t\r\n")
	if strings.HasPrefix(trimmed, "WEBVTT") {
		return parseBlocks(data, true)
	}
	return parseBlocks(data, false)
}

// parseTimestamp 解析 "HH:MM:SS,mmm" 或 "HH:MM:SS.mmm" / "MM:SS.mmm"。
func parseTimestamp(s string) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.Replace(s, ",", ".", 1)
	parts := strings.Split(s, ":")
	var h, m int64
	var secPart string
	switch len(parts) {
	case 2:
		secPart = parts[1]
		if _, err := fmt.Sscanf(parts[0], "%d", &m); err != nil {
			return 0, fmt.Errorf("bad minute %q", parts[0])
		}
	case 3:
		secPart = parts[2]
		if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
			return 0, fmt.Errorf("bad hour %q", parts[0])
		}
		if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
			return 0, fmt.Errorf("bad minute %q", parts[1])
		}
	default:
		return 0, fmt.Errorf("bad timestamp %q", s)
	}
	split := strings.SplitN(secPart, ".", 2)
	if len(split) != 2 || len(split[1]) != 3 {
		return 0, fmt.Errorf("bad milliseconds in %q", s)
	}
	var sec, ms int64
	if _, err := fmt.Sscanf(split[0], "%d", &sec); err != nil {
		return 0, fmt.Errorf("bad second %q", split[0])
	}
	if _, err := fmt.Sscanf(split[1], "%d", &ms); err != nil {
		return 0, fmt.Errorf("bad millis %q", split[1])
	}
	if m >= 60 || sec >= 60 || h > 99 {
		return 0, fmt.Errorf("field out of range in %q", s)
	}
	return timeline.MillisToTicks(((h*60+m)*60+sec)*1000 + ms), nil
}

// parseTiming 解析 "-->" 时间行，返回起止刻度。
func parseTiming(text string) (int64, int64, error) {
	parts := strings.SplitN(text, "-->", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("missing -->")
	}
	// WebVTT 时间行尾部可能带位置设置，按空白截断
	endField := strings.Fields(strings.TrimSpace(parts[1]))
	if len(endField) == 0 {
		return 0, 0, fmt.Errorf("missing end time")
	}
	start, err := parseTimestamp(parts[0])
	if err != nil {
		return 0, 0, err
	}
	end, err := parseTimestamp(endField[0])
	if err != nil {
		return 0, 0, err
	}
	if end <= start {
		return 0, 0, fmt.Errorf("end not after start")
	}
	return start, end, nil
}

// parseBlocks 逐块解析；块间以空行分隔。任何块失败仅记录错误并继续。
func parseBlocks(data []byte, vtt bool) Result {
	res := Result{Format: "srt"}
	if vtt {
		res.Format = "webvtt"
	}
	lines := splitLines(data)
	i := 0
	if vtt { // 跳过 WEBVTT 头（到首个空行）
		for i < len(lines) && strings.TrimSpace(lines[i].text) != "" {
			i++
		}
		for i < len(lines) && strings.TrimSpace(lines[i].text) == "" {
			i++
		}
	}
	var prevEnd int64 = -1
	cueIdx := 0
	for i < len(lines) {
		// 跳过空行
		if strings.TrimSpace(lines[i].text) == "" {
			i++
			continue
		}
		blockStart := lines[i].offset
		// 收集一个块（到空行或 EOF）
		var block []line
		for i < len(lines) && strings.TrimSpace(lines[i].text) != "" {
			block = append(block, lines[i])
			i++
		}
		blockEnd := block[len(block)-1].offset + int64(len(block[len(block)-1].text)) + 1
		j := 0
		// SRT 首行为序号；VTT 可能带 cue 标识行（不含 -->）
		if !strings.Contains(block[j].text, "-->") {
			if len(block) == 1 {
				res.Errors = append(res.Errors, ParseError{Offset: block[j].offset, Kind: "truncated",
					Detail: "block ends without timing line"})
				continue
			}
			j++
		}
		if !strings.Contains(block[j].text, "-->") {
			res.Errors = append(res.Errors, ParseError{Offset: block[j].offset, Kind: "truncated",
				Detail: "missing timing line"})
			continue
		}
		start, end, err := parseTiming(block[j].text)
		if err != nil {
			res.Errors = append(res.Errors, ParseError{Offset: block[j].offset, Kind: "invalid_time",
				Detail: err.Error()})
			continue
		}
		var textLines []string
		for _, l := range block[j+1:] {
			textLines = append(textLines, l.text)
		}
		if len(textLines) == 0 {
			res.Errors = append(res.Errors, ParseError{Offset: block[j].offset, Kind: "truncated",
				Detail: "cue has no text"})
			continue
		}
		if prevEnd >= 0 && start < prevEnd {
			res.Errors = append(res.Errors, ParseError{Offset: block[j].offset, Kind: "overlap",
				Detail: fmt.Sprintf("start %d overlaps previous end %d", start, prevEnd)})
			// 重叠字幕仍保留，便于后续人工对齐
		}
		prevEnd = end
		cueIdx++
		res.Cues = append(res.Cues, Cue{Index: cueIdx, Start: start, End: end,
			Text: strings.Join(textLines, "\n"), ByteStart: blockStart, ByteEnd: blockEnd})
	}
	// 末尾数据被截断（最后一行无换行且块不完整）由块级检查覆盖
	return res
}
