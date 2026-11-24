package manifest

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"mediatime/internal/clock"
)

// ParseHLS 解析一个 HLS 媒体清单（M3U8 点播/事件播放列表）。
//
// 支持 #EXTINF、#EXT-X-MEDIA-SEQUENCE、#EXT-X-TARGETDURATION、
// #EXT-X-DISCONTINUITY；遇到重复序号但 URI 或时长不同的片段直接拒绝整单。
func ParseHLS(data []byte) (*Manifest, error) {
	if len(data) > MaxManifestBytes {
		return nil, ErrManifestTooLarge
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), MaxManifestBytes)

	var (
		seenHeader    bool
		mediaSeq      int
		targetDurTick int64
		frags         []Fragment
		pendingDur    = int64(-1)
		discontinuity bool
		uriBySeq      = map[int]string{}
		durBySeq      = map[int]int64{}
	)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT") {
			switch {
			case line == "#EXTM3U":
				seenHeader = true
			case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
				secs, err := parseTagInt(line)
				if err != nil {
					return nil, fmt.Errorf("%w: line %d: %v", ErrInvalidManifest, lineNo, err)
				}
				targetDurTick = int64(secs) * int64(clock.TickRate)
			case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
				v, err := parseTagInt(line)
				if err != nil {
					return nil, fmt.Errorf("%w: line %d: %v", ErrInvalidManifest, lineNo, err)
				}
				// 清单中途把媒体序号重置到已占用区间会让后续片段复用序号 → 冲突。
				if len(frags) > 0 && v < mediaSeq+len(frags) {
					return nil, fmt.Errorf("%w: media sequence %d already declared", ErrDuplicateSequence, v)
				}
				mediaSeq = v
			case line == "#EXT-X-DISCONTINUITY":
				discontinuity = true
			case strings.HasPrefix(line, "#EXTINF:"):
				secs, err := parseExtInf(line)
				if err != nil {
					return nil, fmt.Errorf("%w: line %d: %v", ErrInvalidManifest, lineNo, err)
				}
				pendingDur = int64(clock.SecondsToTick(secs))
			case strings.HasPrefix(line, "#EXT-X-ENDLIST"),
				strings.HasPrefix(line, "#EXT-X-VERSION"),
				strings.HasPrefix(line, "#EXT-X-PLAYLIST-TYPE"),
				strings.HasPrefix(line, "#EXT-X-ALLOW-CACHE"),
				strings.HasPrefix(line, "#EXT-X-INDEPENDENT-SEGMENTS"):
				// 已知但无需处理的声明性标签。
			default:
				// 未知 EXT 标签忽略，保证前向兼容。
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue // 普通注释
		}
		// 片段 URI 行：必须先有 EXTINF。
		if !seenHeader {
			return nil, fmt.Errorf("%w: line %d: URI before #EXTM3U", ErrInvalidManifest, lineNo)
		}
		if pendingDur < 0 {
			return nil, fmt.Errorf("%w: line %d: segment URI without EXTINF", ErrInvalidManifest, lineNo)
		}
		if err := validatePath(line); err != nil {
			return nil, fmt.Errorf("%w: sequence %d", err, mediaSeq+len(frags))
		}
		// 片段清单序号以已接受片段数为准；媒体序号回退会让新片段落到
		// 已存在的媒体序号上，此时按“重复序号内容冲突”规则处理。
		index := len(frags)
		seq := mediaSeq + index
		if oldURI, ok := uriBySeq[seq]; ok {
			if oldURI != line || durBySeq[seq] != pendingDur {
				return nil, fmt.Errorf("%w: sequence %d", ErrDuplicateSequence, seq)
			}
			// 完全相同的重复声明：跳过，不重复计数。
			pendingDur = -1
			discontinuity = false
			continue
		}
		var start int64
		if n := len(frags); n > 0 {
			start = frags[n-1].StartTicks + frags[n-1].DurationTicks
		}
		frags = append(frags, Fragment{
			Sequence:      len(frags),
			URI:           line,
			DurationTicks: pendingDur,
			Discontinuity: discontinuity,
			StartTicks:    start,
		})
		uriBySeq[seq] = line
		durBySeq[seq] = pendingDur
		pendingDur = -1
		discontinuity = false
		if err := checkFragmentCount(len(frags)); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if !seenHeader {
		return nil, fmt.Errorf("%w: missing #EXTM3U header", ErrInvalidManifest)
	}
	if len(frags) == 0 {
		return nil, fmt.Errorf("%w: no media segments", ErrInvalidManifest)
	}
	return &Manifest{
		Protocol:       ProtoHLS,
		TargetDuration: targetDurTick,
		Fragments:      frags,
		MediaSequence:  mediaSeq,
	}, nil
}

func parseTagInt(line string) (int, error) {
	body := line[strings.Index(line, ":")+1:]
	// 去掉可能的属性列表。
	if i := strings.Index(body, ","); i >= 0 {
		body = body[:i]
	}
	v, err := strconv.Atoi(strings.TrimSpace(body))
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, strconv.ErrSyntax
	}
	return v, nil
}

func parseExtInf(line string) (float64, error) {
	body := line[len("#EXTINF:"):]
	if i := strings.Index(body, ","); i >= 0 {
		body = body[:i]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(body), 64)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, strconv.ErrSyntax
	}
	return v, nil
}
