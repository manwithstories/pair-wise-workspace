package manifest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"review/internal/fault"
)

// HLSSegment 一个 HLS 分片。
type HLSSegment struct {
	URI           string
	Path          string // 解析后的本地路径
	Duration      float64
	Discontinuity bool // 前置 EXT-X-DISCONTINUITY
	Seq           int
}

// HLS 解析结果。
type HLS struct {
	Segments            []HLSSegment
	InitPath            string
	Timescale           int64
	DiscontinuityCount  int
}

// ParseHLS 解析 m3u8，记录 discontinuity，校验 init 段与路径逃逸。
func ParseHLS(path string) (*HLS, []*fault.Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []*fault.Error{fault.Newf(fault.KindMissingInit, path, 0, false, "read m3u8: %v", err)}
	}
	baseDir := filepath.Dir(path)
	out := &HLS{Timescale: 90000}
	var errs []*fault.Error
	var pendingDur float64
	haveDur := false
	discoNext := false
	lines := strings.Split(string(data), "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		off := int64(strings.Index(string(data), line))
		switch {
		case line == "" || line == "#EXTM3U":
		case strings.HasPrefix(line, "#EXT-X-TIMESCALE:"):
			v, err := strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-TIMESCALE:"), 10, 64)
			if err != nil || v <= 0 {
				errs = append(errs, fault.Newf(fault.KindLimit, path, off, false, "bad timescale %q", line))
				continue
			}
			out.Timescale = v
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			uri := strings.TrimPrefix(line, "#EXT-X-MAP:")
			uri = strings.Trim(strings.TrimPrefix(uri, "URI="), `"`)
			p, ferr := Resolve(baseDir, uri, "", 0)
			if ferr != nil {
				errs = append(errs, ferr)
				continue
			}
			if _, err := os.Stat(p); err != nil {
				errs = append(errs, fault.Newf(fault.KindMissingInit, "hls:init", off, true,
					"init %s missing", p))
				continue
			}
			out.InitPath = p
		case line == "#EXT-X-DISCONTINUITY":
			discoNext = true
			out.DiscontinuityCount++
		case strings.HasPrefix(line, "#EXTINF:"):
			v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64)
			if err != nil {
				errs = append(errs, fault.Newf(fault.KindLimit, path, off, false, "bad EXTINF %q", line))
				continue
			}
			pendingDur, haveDur = v, true
		case strings.HasPrefix(line, "#"):
			// 其他标签忽略
		default:
			if !haveDur {
				errs = append(errs, fault.Newf(fault.KindLimit, line, off, false, "segment without EXTINF"))
				continue
			}
			if len(out.Segments) >= MaxTracks*64 {
				errs = append(errs, fault.Newf(fault.KindLimit, line, off, false, "too many segments"))
				break
			}
			p, ferr := Resolve(baseDir, line, "", 0)
			if ferr != nil {
				errs = append(errs, ferr)
				continue
			}
			out.Segments = append(out.Segments, HLSSegment{
				URI: line, Path: p, Duration: pendingDur,
				Discontinuity: discoNext, Seq: len(out.Segments),
			})
			discoNext, haveDur = false, false
		}
		_ = i
	}
	return out, errs
}
