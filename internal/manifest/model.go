// Package manifest 定义 DASH MPD 清单模型、校验规则与 ISO 8601 时长解析。
package manifest

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"dashpack/internal/errs"
)

// 支持的 timescale：视频 90000，音频 48000。
const (
	VideoTimescale = 90000
	AudioTimescale = 48000
)

// SegmentTemplate 描述分片命名模板与默认时长。
type SegmentTemplate struct {
	Timescale      int64  // 每秒 tick 数
	Duration       int64  // 单片标称时长（tick）
	StartNumber    int64  // 首片编号，从 1 起
	Media          string // 媒体分片模板
	Initialization string // 初始化分片模板
}

// Representation 一路表示层（一档码率或一路音频）。
type Representation struct {
	ID         string
	Bandwidth  int64
	Codecs     string
	ContentType string // 冗余自所属 AdaptationSet，便于校验
	Template   SegmentTemplate
	// Segments 为逐片时长（tick），长度即片数；由对齐/组装阶段填充。
	Segments []int64
}

// AdaptationSet 一组同内容类型的表示层。
type AdaptationSet struct {
	ContentType    string // "video" 或 "audio"
	Representations []Representation
}

// Period 一个播放周期。
type Period struct {
	DurationNs      int64 // Period@duration，整数纳秒
	AdaptationSets  []AdaptationSet
}

// MPD 顶层清单模型。
type MPD struct {
	Type           string // "static"
	MinBufferTimeNs int64
	Periods        []Period
}

var durationRe = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:([0-9]+(?:\.[0-9]+)?)S)?)?$`)

// ParseDuration 解析 ISO 8601 时长（PnDTnHnMnS，支持小数秒）为整数纳秒。
// 空值、负值、超 24 小时、非法字符一律报 E_DURATION。
func ParseDuration(s string) (int64, error) {
	const maxNs = 24 * int64(3600) * 1e9
	if s == "" {
		return 0, errs.New(errs.EDuration, errs.ExitUsage, "empty duration")
	}
	if strings.HasPrefix(s, "-") {
		return 0, errs.New(errs.EDuration, errs.ExitUsage, "negative duration %q", s)
	}
	m := durationRe.FindStringSubmatch(s)
	if m == nil {
		return 0, errs.New(errs.EDuration, errs.ExitUsage, "invalid duration %q", s)
	}
	if m[1] == "" && m[2] == "" && m[3] == "" && m[4] == "" {
		return 0, errs.New(errs.EDuration, errs.ExitUsage, "duration %q has no fields", s)
	}
	var total int64
	atoi := func(v string) int64 {
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	total += atoi(m[1]) * 24 * 3600 * 1e9
	total += atoi(m[2]) * 3600 * 1e9
	total += atoi(m[3]) * 60 * 1e9
	if m[4] != "" {
		sec, err := strconv.ParseFloat(m[4], 64)
		if err != nil {
			return 0, errs.New(errs.EDuration, errs.ExitUsage, "invalid seconds in %q", s)
		}
		total += int64(sec * 1e9)
	}
	if total > maxNs {
		return 0, errs.New(errs.EDuration, errs.ExitUsage, "duration %q exceeds 24h", s)
	}
	return total, nil
}

// FormatDuration 把整数纳秒格式化为 ISO 8601 时长。
func FormatDuration(ns int64) string {
	if ns == 0 {
		return "PT0S"
	}
	h := ns / (3600 * 1e9)
	ns -= h * 3600 * 1e9
	m := ns / (60 * 1e9)
	ns -= m * 60 * 1e9
	s := ns / 1e9
	rem := ns % 1e9
	var b strings.Builder
	b.WriteString("PT")
	if h > 0 {
		fmt.Fprintf(&b, "%dH", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dM", m)
	}
	if rem == 0 {
		fmt.Fprintf(&b, "%dS", s)
	} else {
		frac := fmt.Sprintf("%09d", rem)
		frac = strings.TrimRight(frac, "0")
		fmt.Fprintf(&b, "%d.%sS", s, frac)
	}
	return b.String()
}

// Validate 强制必填字段：Representation 的 id/bandwidth/codecs、
// AdaptationSet 的 contentType、SegmentTemplate 全字段；id 不得重复。
func (m *MPD) Validate() error {
	seen := map[string]bool{}
	if len(m.Periods) == 0 {
		return errs.New(errs.EManifest, errs.ExitUsage, "no period")
	}
	for pi, p := range m.Periods {
		for ai, as := range p.AdaptationSets {
			if as.ContentType == "" {
				return errs.New(errs.EManifest, errs.ExitUsage,
					"period %d adaptationSet %d: missing contentType", pi, ai)
			}
			for ri, r := range as.Representations {
				where := fmt.Sprintf("period %d adaptationSet %d representation %d", pi, ai, ri)
				if r.ID == "" {
					return errs.New(errs.EManifest, errs.ExitUsage, "%s: missing id", where)
				}
				if seen[r.ID] {
					return errs.New(errs.EManifest, errs.ExitUsage, "duplicate representation id %q", r.ID)
				}
				seen[r.ID] = true
				if r.Bandwidth <= 0 {
					return errs.New(errs.EManifest, errs.ExitUsage, "%s: missing bandwidth", where)
				}
				if r.Codecs == "" {
					return errs.New(errs.EManifest, errs.ExitUsage, "%s: missing codecs", where)
				}
				t := r.Template
				if t.Timescale <= 0 || t.Duration <= 0 || t.StartNumber < 1 ||
					t.Media == "" || t.Initialization == "" {
					return errs.New(errs.EManifest, errs.ExitUsage,
						"%s: incomplete SegmentTemplate", where)
				}
			}
		}
	}
	return nil
}
