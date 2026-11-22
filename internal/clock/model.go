// Package clock 定义全流水线共享的时间基准与统一错误模型。
//
// 所有表示层（视频帧、音频块、PSI 事件）都归一到 90kHz 的 Ticks 上，
// 故障统一以 Fault 记录，携带首个异常包的字节偏移，供值班人员定位。
package clock

import "fmt"

// ClockRate 是 MPEG-TS PCR/PTS 使用的 90kHz 基准频率。
const ClockRate = 90000

// Ticks 表示 90kHz 时钟刻度，单调递增（回绕已被展开）。
type Ticks int64

// MsToTicks 将毫秒转换为 90kHz 刻度。
func MsToTicks(ms int64) Ticks { return Ticks(ms * (ClockRate / 1000)) }

// Ms 将刻度截断转换为毫秒。
func (t Ticks) Ms() int64 { return int64(t) / (ClockRate / 1000) }

// NoPosition 表示故障记录中"无包上下文"的占位值。
const NoPosition = -1

// FaultKind 枚举所有可定位故障类型。
type FaultKind string

const (
	FaultPCRWrap           FaultKind = "pcr-wrap"
	FaultCCGap             FaultKind = "cc-gap"
	FaultCCDuplicate       FaultKind = "cc-duplicate"
	FaultCCConflict        FaultKind = "cc-conflict"
	FaultTruncatedPacket   FaultKind = "truncated-packet"
	FaultSyncLoss          FaultKind = "sync-loss"
	FaultPSIVersion        FaultKind = "psi-version"
	FaultPSIIncomplete     FaultKind = "truncated-psi"
	FaultAudioRateSwitch   FaultKind = "audio-rate"
	FaultChannelMapMissing FaultKind = "channel-map-missing"
	FaultColorConflict     FaultKind = "color-conflict"
	FaultBoundaryMismatch  FaultKind = "boundary-mismatch"
	FaultPacketLoss        FaultKind = "packet-loss"
)

// Fault 是统一错误模型的单条记录。
type Fault struct {
	Kind      FaultKind `json:"kind"`
	Offset    int64     `json:"offset"` // 首个异常包的字节偏移；无包上下文时为 NoPosition
	PID       int32     `json:"pid"`    // 无 PID 上下文时为 NoPosition
	Detail    string    `json:"detail"`
	Recovered bool      `json:"recovered"` // 是否已被流水线恢复
}

func (f Fault) Error() string {
	return fmt.Sprintf("%s at offset=%d pid=%d: %s", f.Kind, f.Offset, f.PID, f.Detail)
}

// Faults 是有序故障列表，提供定位辅助。
type Faults []Fault

// FirstOffset 返回列表中最早出现的包偏移（无包上下文时返回 NoPosition）。
func (fs Faults) FirstOffset() int64 {
	best := int64(-1)
	for _, f := range fs {
		if f.Offset < 0 {
			continue
		}
		if best < 0 || f.Offset < best {
			best = f.Offset
		}
	}
	return best
}

// Unrecovered 返回尚未恢复的故障子集。
func (fs Faults) Unrecovered() Faults {
	var out Faults
	for _, f := range fs {
		if !f.Recovered {
			out = append(out, f)
		}
	}
	return out
}

// Base 是共享时间基准：各路输入的本地时间减去各自原点后对齐到同一时间轴。
type Base struct {
	Origin Ticks
}

// Align 将本地刻度归一到共享基准。
func (b Base) Align(local Ticks) Ticks { return local - b.Origin }
