// Package clock 定义全流水线共享的统一采样时钟：
// RTP 序号(16bit)与时间戳(32bit)的回绕展开、采样时间线放置规则与误差类型。
package clock

import (
	"errors"
	"fmt"
)

// ErrorKind 是可机读的错误分类，流水线各阶段据此映射退出码与恢复动作。
type ErrorKind string

const (
	KindTruncatedHeader    ErrorKind = "truncated_header"    // WAV 头截断
	KindNonIntegralFrame   ErrorKind = "non_integral_frame"  // 数据长度非整帧
	KindFormatChanged      ErrorKind = "format_changed"      // 采样率/声道数中途变化
	KindUnsupportedPayload ErrorKind = "unsupported_payload" // 非约定 RTP 负载类型
	KindDuplicateConflict  ErrorKind = "duplicate_conflict"  // 重复序号但内容冲突
	KindBurstLoss          ErrorKind = "burst_loss"          // 突发丢包形成的缺口
	KindTimestampWrap      ErrorKind = "timestamp_wrap"      // 32bit 时间戳回绕
	KindClockDrift         ErrorKind = "clock_drift"         // 设备时钟漂移
	KindDuplicateTimestamp ErrorKind = "duplicate_timestamp" // 重复/倒退时间戳
	KindIndeterminate      ErrorKind = "indeterminate_gap"   // 不可判定的缺口
	KindCacheLimit         ErrorKind = "cache_limit"         // 缓存超限触发检查点
	KindCheckpointMismatch ErrorKind = "checkpoint_digest_mismatch"
	KindIndexSeek          ErrorKind = "index_seek_mismatch" // 随机跳转读回不一致
	KindManifestAlignment  ErrorKind = "manifest_alignment"  // 清单与索引时间基准不一致
	KindInputTooLarge      ErrorKind = "input_too_large"
	KindPathEscape         ErrorKind = "path_escape"
)

const (
	// NoSeq 表示错误不关联 RTP 包序号。
	NoSeq = ^uint64(0)
	// NoOffset 表示错误不关联源字节偏移。
	NoOffset = int64(-1)
)

// StageError 是所有阶段共用的结构化错误，始终携带阶段名与源块字节偏移，
// 并尽量附带设备标识、扩展 RTP 序号与建议恢复动作。
type StageError struct {
	Stage    string    `json:"stage"`
	Kind     ErrorKind `json:"kind"`
	DeviceID string    `json:"device_id,omitempty"`
	Offset   int64     `json:"source_offset"`
	Seq      uint64    `json:"rtp_seq"`
	Message  string    `json:"message"`
	Recovery string    `json:"recovery"`
	Cause    error     `json:"-"`
}

func (e *StageError) Error() string {
	loc := fmt.Sprintf("stage=%s kind=%s device=%q offset=%d", e.Stage, e.Kind, e.DeviceID, e.Offset)
	if e.Seq != NoSeq {
		loc += fmt.Sprintf(" seq=%d", e.Seq)
	}
	if e.Message != "" {
		loc += " msg=" + e.Message
	}
	if e.Recovery != "" {
		loc += " recovery=" + e.Recovery
	}
	if e.Cause != nil {
		loc += " cause=" + e.Cause.Error()
	}
	return loc
}

func (e *StageError) Unwrap() error { return e.Cause }

// New 构造一个结构化阶段错误。
func New(stage string, kind ErrorKind, deviceID string, offset int64, seq uint64, msg, recovery string) *StageError {
	return &StageError{Stage: stage, Kind: kind, DeviceID: deviceID, Offset: offset,
		Seq: seq, Message: msg, Recovery: recovery}
}

// Wrap 在保留原错误的同时包装为阶段错误。
func Wrap(stage string, kind ErrorKind, deviceID string, offset int64, seq uint64, msg, recovery string, cause error) *StageError {
	return &StageError{Stage: stage, Kind: kind, DeviceID: deviceID, Offset: offset,
		Seq: seq, Message: msg, Recovery: recovery, Cause: cause}
}

// AsStageError 从错误链中取出 *StageError。
func AsStageError(err error) (*StageError, bool) {
	var se *StageError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// RFC 3550 A.1 的序号回绕/乱序门限。
const (
	maxDropout        = 3000 // 正常正向跳变上限
	maxMisorder       = 100  // 有界乱序/重复的逆向跳变门限
	seqModulo         = 1 << 16
	tsModulo    int64 = 1 << 32
)

// SeqExtender 按 RFC 3550 A.1 思路把 16bit RTP 序号展开为 64bit 单调序号。
// maxSeq 只随“确认前进”的包推进，因此乱序/重复包不会污染周期计数。
type SeqExtender struct {
	started bool
	maxSeq  uint16
	cycles  uint64
}

// Update 输入原始 16bit 序号，返回扩展后的 64bit 序号以及本次是否发生回绕。
// 判定基于 16bit 回绕距离：前向距离 <32768 视为前进（含越过 65535），
// 后向距离 <=maxMisorder 视为有界乱序/重复，其余为异常跳转且不污染基准态。
func (s *SeqExtender) Update(seq uint16) (extended uint64, wrapped bool) {
	if !s.started {
		s.started = true
		s.maxSeq = seq
		return uint64(seq), false
	}
	fwd := (uint32(seq) - uint32(s.maxSeq)) & 0xffff // 16bit 回绕的前向距离
	switch {
	case fwd == 0:
		// 重复序号，基准态不变。
	case fwd < 1<<15 && fwd <= maxDropout:
		// 确认前进；跨越 0 边界即回绕。
		if seq < s.maxSeq {
			s.cycles++
			wrapped = true
		}
		s.maxSeq = seq
	case (1<<16 - fwd) <= maxMisorder:
		// 有界乱序/迟到包：基准态不变，按当前周期解释。
	default:
		// 异常大跳转：不改变基准态，缺口交由重排层处理。
	}
	return s.cycles*seqModulo + uint64(seq), wrapped
}

// Cycles 返回已观测到的回绕次数。
func (s *SeqExtender) Cycles() uint64 { return s.cycles }

// TSUnwrapper 把 32bit RTP 时间戳展开为 64bit 有符号单调时间线（单位仍为采样时钟 tick）。
type TSUnwrapper struct {
	started bool
	last    uint32
	total   int64
}

// Update 输入 32bit 时间戳，返回展开后的 64bit 值以及本次是否发生回绕。
func (u *TSUnwrapper) Update(ts uint32) (extended int64, wrapped bool) {
	if !u.started {
		u.started = true
		u.last = ts
		u.total = int64(int32(ts))
		return u.total, false
	}
	// 回绕：本次为真实前进（int32 差值为正），但原始 32bit 数值变小（越过 0 边界）。
	delta := int32(ts - u.last)
	if delta > 0 && ts < u.last {
		wrapped = true
	}
	u.total += int64(delta)
	u.last = ts
	return u.total, wrapped
}

// SamplesToMS 把采样数换算为毫秒（浮点），用于报告与对齐误差。
func SamplesToMS(samples int64, sampleRate uint32) float64 {
	if sampleRate == 0 {
		return 0
	}
	return float64(samples) * 1000 / float64(sampleRate)
}

// UncertainRange 显式标注时间线上不可完全确定的采样区间，
// 流水线禁止通过压缩时间线来“消除”这类区间。
type UncertainRange struct {
	DeviceID     string    `json:"device_id"`
	StartSample  int64     `json:"start_sample"`
	EndSample    int64     `json:"end_sample"`
	Kind         ErrorKind `json:"kind"`
	Reason       string    `json:"reason"`
	SourceOffset int64     `json:"source_offset"`
}

// Length 返回区间覆盖的采样数。
func (r UncertainRange) Length() int64 { return r.EndSample - r.StartSample }

// DriftToleranceSamples 是单块允许的时钟漂移容差（采样点）。
const DriftToleranceSamples = 2

// Timeline 是单设备的统一采样时钟：所有修复后的采样块都在该时间线上取得绝对起点。
type Timeline struct {
	DeviceID   string
	SampleRate uint32

	unw        TSUnwrapper
	started    bool
	first      int64
	next       int64
	lastRTPTS  int64
	Uncertain  []UncertainRange
	Repaired   int // 被纠正的时间戳数量（漂移/重复/回绕修正）
	WrapEvents int // 时间戳回绕事件数
}

// NewTimeline 创建一条锚定在采样点 0 的设备时间线。
func NewTimeline(deviceID string, sampleRate uint32) *Timeline {
	return &Timeline{DeviceID: deviceID, SampleRate: sampleRate}
}

// Started 报告时间线是否已经放置过采样块。
func (t *Timeline) Started() bool { return t.started }

// NextSample 返回下一个采样块应当放置的绝对采样位置。
func (t *Timeline) NextSample() int64 { return t.next }

// FirstSample 返回首个采样块的绝对起点。
func (t *Timeline) FirstSample() int64 { return t.first }

// MarkUncertain 记录一个不确定区间。
func (t *Timeline) MarkUncertain(start, end int64, kind ErrorKind, reason string, offset int64) UncertainRange {
	r := UncertainRange{DeviceID: t.DeviceID, StartSample: start, EndSample: end,
		Kind: kind, Reason: reason, SourceOffset: offset}
	t.Uncertain = append(t.Uncertain, r)
	return r
}

// InsertGap 在当前游标处插入长度为 missing 的缺口（时间线保持不压缩），
// 用于 RTP 层已确认的突发丢包；返回缺口区间。
func (t *Timeline) InsertGap(missing int64, kind ErrorKind, reason string, offset int64) UncertainRange {
	if missing <= 0 {
		missing = 1
	}
	r := t.MarkUncertain(t.next, t.next+missing, kind, reason, offset)
	t.next += missing
	return r
}

// PlaceResult 描述一次时间线放置的结果。
type PlaceResult struct {
	StartSample int64
	Repaired    bool
	Uncertain   *UncertainRange
}

// Place 依据展开后的 RTP 时间戳与可信采样计数放置一个采样块。
// 时间线始终按真实采样计数前进，任何缺口都显式保留，绝不静默压缩。
func (t *Timeline) Place(rtpTS int64, sampleCount int64, offset int64) PlaceResult {
	if sampleCount <= 0 {
		sampleCount = 0
	}
	if !t.started {
		t.started = true
		t.first = 0
		t.next = sampleCount
		t.lastRTPTS = rtpTS
		return PlaceResult{StartSample: 0}
	}
	delta := rtpTS - t.lastRTPTS
	start := t.next
	res := PlaceResult{StartSample: start}
	switch {
	case delta == sampleCount:
		// 与采样计数严格一致，无需修复。
	case delta > sampleCount:
		// 时间戳声称的间隔大于本块采样数。突发丢包缺口统一由 RTP 序号层
		// (Reconciler.consumeGaps -> InsertGap) 声明；此处只记录时间戳层面的
		// 证据，不重复推进游标，避免同一缺口被双重计数。
		r := t.MarkUncertain(t.next, t.next, KindBurstLoss,
			"timestamp gap corroborates rtp sequence loss", offset)
		res.Uncertain = &r
	case delta == 0:
		// 重复时间戳：信任采样计数顺序放置，标记为修复。
		r := t.MarkUncertain(start, start, KindDuplicateTimestamp,
			"duplicate rtp timestamp, trusted sample count", offset)
		res.Uncertain = &r
		res.Repaired = true
		t.Repaired++
	case delta < 0:
		// 时间戳倒退：小幅倒退按漂移修复；大幅倒退不可判定，显式标注。
		res.Repaired = true
		t.Repaired++
		if delta < -sampleCount*10 {
			r := t.MarkUncertain(start, start+sampleCount, KindIndeterminate,
				"large backward timestamp jump, gap length undecidable", offset)
			res.Uncertain = &r
		} else {
			r := t.MarkUncertain(start, start, KindClockDrift,
				"backward rtp timestamp corrected by sample count", offset)
			res.Uncertain = &r
		}
	default:
		// 0 < delta < sampleCount：块间重叠/轻微漂移，信任采样计数。
		res.Repaired = true
		t.Repaired++
		r := t.MarkUncertain(start, start, KindClockDrift,
			"rtp timestamp drift corrected by trusted sample count", offset)
		res.Uncertain = &r
	}
	t.next = start + sampleCount
	t.lastRTPTS = rtpTS
	return res
}

// NoteWrap 由修复层在观测到时间戳回绕时调用，仅用于统计。
func (t *Timeline) NoteWrap() { t.WrapEvents++ }

// TotalSamples 返回时间线当前覆盖的总采样数（含缺口）。
func (t *Timeline) TotalSamples() int64 {
	if !t.started {
		return 0
	}
	return t.next - t.first
}

// SeedRange 在验证阶段从清单重建时间线游标，不经过 Place()。
func (t *Timeline) SeedRange(first, next int64) {
	t.started = true
	t.first = first
	t.next = next
}
