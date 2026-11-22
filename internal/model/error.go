// Package model 定义管线各阶段共享的统一错误模型：每个事件都带有
// 统一采样刻度上的时间范围、来源 RTP 包序号与严重度。
package model

import (
	"encoding/json"
	"fmt"
)

// Severity 描述事件的严重程度。
type Severity string

const (
	// SevInfo 仅提示，不影响数据可用性。
	SevInfo Severity = "info"
	// SevWarning 数据可恢复或降级可用。
	SevWarning Severity = "warning"
	// SevError 数据损坏且不可恢复，对应损坏区段。
	SevError Severity = "error"
)

// Code 标识事件种类，便于跨阶段聚合与测试断言。
type Code string

const (
	CodeTruncated      Code = "wav_truncated"       // WAV 容器截断
	CodeBadChunk       Code = "wav_bad_chunk"        // 越界/畸形 RIFF 块
	CodeBadFormat      Code = "wav_bad_format"       // 非法声道/格式/样本帧
	CodeDupConflict    Code = "rtp_dup_conflict"     // 同序号载荷冲突
	CodeLateOutside    Code = "rtp_late_outside"     // 窗口外迟到，不可恢复
	CodeBurstLoss      Code = "rtp_burst_loss"       // 突发连续丢包
	CodeTimestampRoll  Code = "rtp_timestamp_rollback" // RTP 时间戳回退
	CodeRecovered      Code = "gap_recovered"        // 缺口经冗余校验恢复
	CodeUnrecoverable  Code = "gap_unrecoverable"    // 无一致冗余，禁止补零
	CodeAVDiscontinuity Code = "av_discontinuity"    // 分片时间戳不连续/跨关键帧
)

// Range 是统一采样刻度上的闭开区间 [Start, End)，单位为绑定时钟的样本数。
type Range struct {
	Start uint64 `json:"start_sample"`
	End   uint64 `json:"end_sample"`
}

// Empty 判断区间是否为空。
func (r Range) Empty() bool { return r.End <= r.Start }

// Len 返回区间长度，做溢出保护。
func (r Range) Len() uint64 {
	if r.Empty() {
		return 0
	}
	return r.End - r.Start
}

// Overlap 判断两个区间是否相交。
func (r Range) Overlap(o Range) bool {
	return r.Start < o.End && o.Start < r.End
}

// Merge 返回覆盖两个区间的最小区间。
func (r Range) Merge(o Range) Range {
	out := Range{Start: r.Start, End: r.End}
	if o.Start < out.Start {
		out.Start = o.Start
	}
	if o.End > out.End {
		out.End = o.End
	}
	return out
}

// Event 是管线传播的统一错误/诊断事件。
type Event struct {
	Code     Code     `json:"code"`
	Severity Severity `json:"severity"`
	// 时间范围（统一采样刻度）。零值表示该事件不绑定时间区间。
	TimeRange Range `json:"time_range"`
	// 来源 RTP 包序号（16 位回绕序号）。-1 表示不适用。
	SeqStart int `json:"seq_start"`
	SeqEnd   int `json:"seq_end"`
	// 可选的人类可读细节与结构化上下文。
	Message string `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// NewEvent 构造事件，SeqStart/SeqEnd 缺省为 -1。
func NewEvent(code Code, sev Severity, r Range, msg string) Event {
	return Event{
		Code:      code,
		Severity:  sev,
		TimeRange: r,
		SeqStart:  -1,
		SeqEnd:    -1,
		Message:   msg,
	}
}

// WithSeq 绑定来源包序号区间并返回事件本身（流式调用）。
func (e Event) WithSeq(start, end int) Event {
	e.SeqStart, e.SeqEnd = start, end
	return e
}

// WithDetail 附加结构化上下文。
func (e Event) WithDetail(k string, v any) Event {
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	e.Detail[k] = v
	return e
}

// String 返回单行可读表示。
func (e Event) String() string {
	return fmt.Sprintf("[%s] %s seq=[%d,%d] samples=[%d,%d) %s",
		e.Severity, e.Code, e.SeqStart, e.SeqEnd,
		e.TimeRange.Start, e.TimeRange.End, e.Message)
}

// MarshalJSON 保证 Detail 为 nil 时字段稳定省略。
func (e Event) MarshalJSON() ([]byte, error) {
	type alias Event
	return json.Marshal((alias)(e))
}

// CorruptRange 是最终输出中的损坏区段摘要（成功恢复的缺口不计入）。
type CorruptRange struct {
	StartSample uint64 `json:"start_sample"`
	EndSample   uint64 `json:"end_sample"`
	StartMs     uint64 `json:"start_ms"`
	EndMs       uint64 `json:"end_ms"`
	Codes       []Code `json:"codes"`
}
