package clock

// Mod33 是 PCR/PTS 的 33 位模数（2^33）。
const Mod33 = int64(1) << 33

// half33 是回绕判定半径（2^32）。
const half33 = Mod33 / 2

// Unwrapper 对 33 位 PCR/PTS 原始值做模数回绕展开。
// 相同输入序列必然产生相同输出，重跑结果确定。
type Unwrapper struct {
	init bool
	last int64 // 上一次展开后的绝对值
}

// Unwrap 输入 33 位原始值，返回单调（允许持平）的绝对刻度。
// 相邻两次采样的真实间隔被假定在 [-2^32, 2^32) 内。
func (u *Unwrapper) Unwrap(raw uint64) int64 {
	v := int64(raw & uint64(Mod33-1))
	if !u.init {
		u.init = true
		u.last = v
		return v
	}
	diff := (v - (u.last & (Mod33 - 1))) % Mod33
	if diff < 0 {
		diff += Mod33
	}
	if diff >= half33 {
		diff -= Mod33
	}
	u.last += diff
	return u.last
}

// Last 返回最近一次展开后的绝对刻度。
func (u *Unwrapper) Last() int64 { return u.last }

// WrapCount 返回截至当前检测到的正向回绕次数（用于审计）。
func (u *Unwrapper) WrapCount() int {
	if !u.init {
		return 0
	}
	w := u.last / Mod33
	if w < 0 {
		return 0
	}
	return int(w)
}
