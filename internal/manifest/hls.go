package manifest

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"mediaverify/internal/clock"
	"mediaverify/internal/fault"
)

// ParseHLS 解析 m3u8 媒体播放列表。
// discontinuity 处必须由 EXT-X-PROGRAM-DATE-TIME 给出显式时钟重置映射，
// 否则时间线无法无歧义映射，报错并携带行号。
func ParseHLS(name string, r io.Reader) (*Program, error) {
	tl := clock.NewTimeline()
	rep := Representation{ID: "hls", InitURI: ""}
	var local clock.Ticks // 本地累计时间轴
	var pendingDur clock.Ticks
	var haveDur bool
	var pdtBase *time.Time // 首个 PDT，作为统一时钟原点
	lineNo := 0
	segIdx := 0
	discontinuity := false
	pdtSeen := false
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		lineNo++
		ln := strings.TrimSpace(sc.Text())
		switch {
		case ln == "" || ln == "#EXTM3U":
		case strings.HasPrefix(ln, "#EXT-X-MAP:"):
			uri, ok := mapURI(ln)
			if !ok || uri == "" {
				return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, -1, "malformed EXT-X-MAP")
			}
			rep.InitURI = uri
		case ln == "#EXT-X-DISCONTINUITY":
			discontinuity = true
		case strings.HasPrefix(ln, "#EXT-X-PROGRAM-DATE-TIME:"):
			ts, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(ln, "#EXT-X-PROGRAM-DATE-TIME:"))
			if err != nil {
				return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, -1, "bad PROGRAM-DATE-TIME: %v", err)
			}
			if pdtBase == nil {
				base := ts
				pdtBase = &base
			}
			unified := clock.Ticks(ts.Sub(*pdtBase))
			switch {
			case discontinuity || !pdtSeen:
				// discontinuity 处的显式时钟重置映射（或首个 PDT 建立原点）
				if err := tl.Reset(local, unified); err != nil {
					return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, segIdx,
						"ambiguous clock reset: %v", err)
				}
			default:
				// 非 discontinuity 处 PDT 必须与连续时间线一致
				if want, _ := tl.Map(local); unified != want {
					return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, segIdx,
						"PROGRAM-DATE-TIME jump without discontinuity: got=%d want=%d", unified, want)
				}
			}
			pdtSeen = true
			discontinuity = false // 本 discontinuity 已获得显式映射
		case strings.HasPrefix(ln, "#EXTINF:"):
			f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(ln, "#EXTINF:"), ","), 64)
			if err != nil || f <= 0 {
				return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, segIdx, "bad EXTINF %q", ln)
			}
			pendingDur = clock.Ticks(f * float64(clock.Second))
			haveDur = true
		case strings.HasPrefix(ln, "#"):
			// 其他标签忽略
		default:
			if !haveDur {
				return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, segIdx, "segment URI without EXTINF")
			}
			if discontinuity {
				return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, segIdx,
					"discontinuity without explicit clock reset (PROGRAM-DATE-TIME); timeline ambiguous")
			}
			if rep.InitURI == "" {
				return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, segIdx, "missing init segment (EXT-X-MAP)")
			}
			start, err := tl.Map(local)
			if err != nil {
				return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, segIdx, "%v", err)
			}
			// 重叠边界检查
			if n := len(rep.Segments); n > 0 {
				prev := rep.Segments[n-1]
				if start < prev.Start+prev.Duration {
					return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, segIdx,
						"overlapping boundary: start=%d prev_end=%d", start, prev.Start+prev.Duration)
				}
			}
			rep.Segments = append(rep.Segments, Segment{
				Index: segIdx, URI: ln, Start: start, Duration: pendingDur, RepID: "hls",
			})
			local += pendingDur
			haveDur = false
			segIdx++
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fault.New(fault.KindManifest, fault.Fail, name, lineNo, -1, "scan: %v", err)
	}
	if len(rep.Segments) == 0 {
		return nil, fault.New(fault.KindManifest, fault.Fail, name, 0, -1, "no segments")
	}
	return &Program{Representations: []Representation{rep}, Timeline: tl}, nil
}

func mapURI(ln string) (string, bool) {
	for _, attr := range strings.Split(strings.TrimPrefix(ln, "#EXT-X-MAP:"), ",") {
		kv := strings.SplitN(attr, "=", 2)
		if len(kv) == 2 && kv[0] == "URI" {
			return strings.Trim(kv[1], `"`), true
		}
	}
	return "", false
}

// String 便于调试输出。
func (s Segment) String() string {
	return fmt.Sprintf("seg#%d %s start=%dms dur=%dms", s.Index, s.URI, s.Start/1e6, s.Duration/1e6)
}
