// Package timeline 统一多机位、多轨道的时间基准：每个轨道自带 tick 时钟，
// 全部映射到以纳秒为单位的共同时间线。任何映射失败都必须保留原始 tick 与
// 轨道标识，禁止静默改写时间线。
package timeline

import (
	"fmt"
)

// 纳秒时间线使用的固定时间基准。
const NanoTimescale uint32 = 1_000_000_000

// 限制单个轨道允许声明的最大时间基准，防止畸形 mdhd 触发整数放大。
const MaxTimescale uint32 = 1_000_000_000

// MapError 表示一次 tick -> 纳秒映射失败。
// 失败时 RawTick 与 TrackID 必须原样保留，供上层恢复与审计。
type MapError struct {
	TrackID uint32
	RawTick uint64
	Kind    string
	Reason  string
}

func (e *MapError) Error() string {
	return fmt.Sprintf("timeline map error: track=%d kind=%s raw_tick=%d: %s",
		e.TrackID, e.Kind, e.RawTick, e.Reason)
}

// Clock 描述单个轨道的时间基准：tick 0 对应共同时间线上的 EpochNs，
// 每秒包含 Timescale 个 tick。
type Clock struct {
	TrackID   uint32
	Kind      string
	Timescale uint32
	EpochNs   int64
}

// NewClock 构造轨道时钟。timescale 为 0 时返回的时钟在映射时显式报错。
func NewClock(trackID uint32, kind string, timescale uint32, epochNs int64) (*Clock, error) {
	if timescale > MaxTimescale {
		return nil, &MapError{TrackID: trackID, Kind: kind,
			Reason: fmt.Sprintf("timescale %d exceeds limit %d", timescale, MaxTimescale)}
	}
	return &Clock{TrackID: trackID, Kind: kind, Timescale: timescale, EpochNs: epochNs}, nil
}

// ToNanos 将轨道 tick 映射为共同时间线纳秒，采用四舍五入并标注误差。
// 返回值依次为纳秒时间戳、量化误差（纳秒，绝对值 < 1e9/Timescale）。
func (c *Clock) ToNanos(tick int64) (int64, int64, error) {
	if c == nil || c.Timescale == 0 {
		return 0, 0, &MapError{TrackID: c.safeID(), Kind: c.safeKind(),
			RawTick: uint64(tick), Reason: "clock has zero timescale"}
	}
	num := tick * int64(NanoTimescale)
	ts := int64(c.Timescale)
	quo, rem := num/ts, num%ts
	if rem < 0 {
		// 负数 tick 的四舍五入。
		if -rem*2 >= ts {
			quo--
		}
	} else if rem*2 >= ts {
		quo++
	}
	errNs := num/ts - quo // 舍入引入的误差（rem 派生，始终有界）
	return c.EpochNs + quo, errNs, nil
}

// ToTick 将共同时间线纳秒反向映射为轨道 tick（最近邻）。
func (c *Clock) ToTick(nanos int64) (int64, error) {
	if c == nil || c.Timescale == 0 {
		return 0, &MapError{TrackID: c.safeID(), Kind: c.safeKind(),
			Reason: "clock has zero timescale"}
	}
	rel := nanos - c.EpochNs
	ts := int64(c.Timescale)
	num := rel * ts
	quo, rem := num/int64(NanoTimescale), num%int64(NanoTimescale)
	if rem < 0 {
		if -rem*2 >= int64(NanoTimescale) {
			quo--
		}
	} else if rem*2 >= int64(NanoTimescale) {
		quo++
	}
	return quo, nil
}

func (c *Clock) safeID() uint32 {
	if c == nil {
		return 0
	}
	return c.TrackID
}

func (c *Clock) safeKind() string {
	if c == nil {
		return "unknown"
	}
	return c.Kind
}
