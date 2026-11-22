// Package model 定义全链路共享的统一时间基准、媒体区间、分片模型与结构化错误。
// 所有子系统（解析器、时钟内核、音频处理、再封装、清单、报告）只通过本包的
// Rational 时间与 MediaError 交换时间信息与错误，禁止各自定义时基。
package model

import (
	"errors"
	"fmt"
	"strings"
)

// 统一时间刻度：内部一律以“秒”的有理数表示，避免浮点累计漂移。
// Rational 规范化为 Den > 0；零值表示 0/1。
type Rational struct {
	Num int64
	Den int64
}

func NewRational(num, den int64) Rational {
	if den == 0 {
		panic("model: zero denominator")
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(abs64(num), den)
	if g > 1 {
		num /= g
		den /= g
	}
	return Rational{Num: num, Den: den}
}

// FromTimescale 把某时基下的 tick 数换算为统一秒。
func FromTimescale(ticks int64, timescale uint32) Rational {
	if timescale == 0 {
		panic("model: zero timescale")
	}
	return NewRational(ticks, int64(timescale))
}

func (r Rational) Valid() bool { return r.Den > 0 }

func (r Rational) Add(o Rational) Rational {
	return NewRational(r.Num*o.Den+o.Num*r.Den, r.Den*o.Den)
}

func (r Rational) Sub(o Rational) Rational {
	return NewRational(r.Num*o.Den-o.Num*r.Den, r.Den*o.Den)
}

func (r Rational) Cmp(o Rational) int {
	l := r.Num * o.Den
	rr := o.Num * r.Den
	switch {
	case l < rr:
		return -1
	case l > rr:
		return 1
	default:
		return 0
	}
}

// Ticks 把统一秒换算为目标时基下的 tick（向下取整）。
func (r Rational) Ticks(timescale uint32) int64 {
	if timescale == 0 {
		panic("model: zero timescale")
	}
	// 先约分再乘，降低溢出风险。
	v := NewRational(r.Num*int64(timescale), r.Den)
	return v.Num / v.Den
}

// Seconds 返回浮点秒，仅用于报告展示；决策逻辑不得依赖它。
func (r Rational) Seconds() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

// Milli 返回毫秒浮点，用于清单对齐误差展示。
func (r Rational) Milli() float64 { return r.Seconds() * 1000 }

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	if a == 0 {
		return 1
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ErrorCategory 是可被上层按策略处理的稳定错误类别，禁止用裸字符串错误。
type ErrorCategory string

const (
	ErrTruncatedBox      ErrorCategory = "truncated_box"
	ErrOversizeBox       ErrorCategory = "oversize_box"
	ErrFragmentGap       ErrorCategory = "fragment_gap"
	ErrTFDTRegression    ErrorCategory = "tfdt_regression"
	ErrMissingMFHD       ErrorCategory = "missing_mfhd"
	ErrDuplicateConflict ErrorCategory = "duplicate_conflict"
	ErrUnknownCodec      ErrorCategory = "unknown_codec"
	ErrUnknownLayout     ErrorCategory = "unknown_layout"
	ErrUnsupportedRate   ErrorCategory = "unsupported_sample_rate"
	ErrShortDuration     ErrorCategory = "short_duration"
	ErrManifestAlign     ErrorCategory = "manifest_misalignment"
	ErrPathViolation     ErrorCategory = "path_violation"
	ErrOversizeInput     ErrorCategory = "oversize_input"
	ErrWriteFailed       ErrorCategory = "write_failed"
)

// MediaError 携带来源区间与可定位信息（如截断 box 的字节偏移、轨道号）。
type MediaError struct {
	Category ErrorCategory
	Message  string
	Offset   int64     // 字节偏移，主要用于截断 box 定位
	TrackID  uint32    // 轨道编号，未知布局/编解码场景必填
	Source   *Interval // 触发错误的来源区间，可为 nil
}

func (e *MediaError) Error() string {
	var b strings.Builder
	b.WriteString(string(e.Category))
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Offset != 0 {
		fmt.Fprintf(&b, " (offset=%d)", e.Offset)
	}
	if e.TrackID != 0 {
		fmt.Fprintf(&b, " (track=%d)", e.TrackID)
	}
	if e.Source != nil {
		fmt.Fprintf(&b, " (seq=%d)", e.Source.FragmentSeq)
	}
	return b.String()
}

// AsMediaError 解包错误链中的结构化媒体错误。
func AsMediaError(err error) (*MediaError, bool) {
	var me *MediaError
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}

// Interval 是统一时基下的半开时间区间 [Start, End)，并保留来源定位。
type Interval struct {
	Start       Rational
	End         Rational
	FragmentSeq uint32
	ByteOffset  int64
	ByteLength  int64
}

func (i Interval) Duration() Rational { return i.End.Sub(i.Start) }

// BoxSummary 记录解析到的 box；未知 box 保留并计入摘要。
type BoxSummary struct {
	Type    string
	Offset  int64
	Size    int64
	Depth   int
	Unknown bool
}

// Sample 是跨轨道统一的样本载体：音频为交错前的 PCM/原始帧，视频为帧负载。
type Sample struct {
	TrackID  uint32
	Data     []byte
	Duration int64 // 轨道时基 tick
	Flags    uint32
}

// Fragment 是一个 fMP4 分片解析后的内核视图。
type Fragment struct {
	Sequence            uint32
	Timescale           uint32
	BaseMediaDecodeTime int64
	Intervals           []Interval
	Samples             []Sample
	Boxes               []BoxSummary
	ContentSHA          string // 内容摘要（规范化负载的 sha256 hex）
	RawSize             int64
}

// 支持的采样率与声道布局白名单。
const (
	Rate44100          uint32 = 44100
	Rate48000          uint32 = 48000
	TargetSampleRate   uint32 = 48000
	MinDurationSeconds        = 1 // 不足时长样本判定阈值（秒）
)

type ChannelLayout string

const (
	LayoutStereo ChannelLayout = "stereo"
	Layout51     ChannelLayout = "5.1"
	Layout71     ChannelLayout = "7.1"
)

func ParseLayout(s string) (ChannelLayout, error) {
	switch ChannelLayout(s) {
	case LayoutStereo, Layout51, Layout71:
		return ChannelLayout(s), nil
	default:
		return "", &MediaError{Category: ErrUnknownLayout, Message: "unknown channel layout: " + s}
	}
}

func (l ChannelLayout) Channels() int {
	switch l {
	case LayoutStereo:
		return 2
	case Layout51:
		return 6
	case Layout71:
		return 8
	default:
		return 0
	}
}

func SupportedRate(r uint32) bool { return r == Rate44100 || r == Rate48000 }

// ColorMeta 是视频色彩元数据；冲突时由再封装按配置优先级选择。
type ColorMeta struct {
	ColorPrimaries uint16
	Transfer       uint16
	Matrix         uint16
	FullRange      bool
}

// TrackMeta 是初始化段中解析出的轨道元数据。
type TrackMeta struct {
	TrackID    uint32
	Codec      string
	Timescale  uint32
	SampleRate uint32
	Layout     ChannelLayout
	BitDepth   uint16
	Color      *ColorMeta
}
