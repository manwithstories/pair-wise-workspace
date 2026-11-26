// Package clock 定义统一的 27 MHz 时间基准、时间戳回绕展开、
// 节点漂移估计以及多轨单调时间线。
package clock

// Tick 是 27 MHz 时钟上的一个整数刻度。1 秒 = 27_000_000 Tick。
type Tick int64

// TicksPerSecond 是 MPEG-TS PCR/PTS 体系使用的 27 MHz 基准频率。
const TicksPerSecond Tick = 27_000_000

// Seconds 将 Tick 转换为浮点秒，仅用于报表与渲染标注。
func (t Tick) Seconds() float64 {
	return float64(t) / float64(TicksPerSecond)
}

// Milliseconds 将 Tick 转换为毫秒，便于终端输出。
func (t Tick) Milliseconds() int64 {
	return int64(t) * 1000 / int64(TicksPerSecond)
}

// PCRModulus 是 33+9 位 PCR 基准的回绕模数（2^33 个 90 kHz 单位，
// 再以 300 倍细分到 27 MHz）。
const PCRModulus Tick = 1 << 33

// PTSModulus 是 33 位 PTS/DTS 字段的回绕模数（90 kHz 单位）。
const PTSModulus Tick = 1 << 33

// PCRTicksPerPTS 是一个 90 kHz PTS 单位对应的 27 MHz 刻度数。
const PCRTicksPerPTS Tick = 300

// Unwrap 展开按 modulus 回绕的字段值。
//
// prev 是上一次已展开的值（首次调用传 ok=false），raw 是当前
// 报文中的原始字段值，modulus 是字段回绕模数（PCR/PTS 均为 2^33，
// 但二者的 Tick 粒度不同，由调用方换算后传入）。
// 函数假设相邻采样间隔小于 modulus/2，据此选择最接近 prev 的展开结果。
func Unwrap(prev Tick, raw Tick, modulus Tick, ok bool) Tick {
	if !ok {
		return raw
	}
	// 先对齐到 prev 所在的周期，再在相邻周期中挑选距离最近者。
	cycles := prev / modulus
	base := cycles*modulus + raw
	candidates := []Tick{base - modulus, base, base + modulus}
	best := base
	bestDist := absTick(prev - base)
	for _, c := range candidates {
		d := absTick(prev - c)
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	return best
}

func absTick(v Tick) Tick {
	if v < 0 {
		return -v
	}
	return v
}

// PTSFrom90k 将 90 kHz 的 PTS 字段换算为 27 MHz Tick。
func PTSFrom90k(pts90k Tick) Tick {
	return pts90k * PCRTicksPerPTS
}
