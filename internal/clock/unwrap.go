package clock

// Unwrapper 将 32 位回绕时间戳扩展为单调的 64 位 tick 序列。
// 算法参照 RFC 3550 附录 A.1 的回绕判据：以半程为门限选择最短有符号跳变。
type Unwrapper struct {
	modulus     int64
	half        int64
	baseRaw     uint32 // 首帧原始时间戳，作为扩展基准
	acc         int64  // 相对首帧的累计有符号 delta
	lastRaw     uint32
	initialized bool
}

// NewUnwrapper 构造扩展器，modulus 为回绕模（0 表示 2^32）。
func NewUnwrapper(modulus uint64) *Unwrapper {
	m := int64(modulus)
	if m <= 0 {
		m = 1 << 32
	}
	return &Unwrapper{modulus: m, half: m / 2}
}

// Extend 输入一个原始时间戳，返回扩展后的 64 位 tick 以及是否发生倒退。
// 扩展值 = 首帧基准 + 相邻帧最短有符号跳变的累计和；
// 正常前向回绕（如 max-10 -> 5）会计入一个完整模长，倒退仅置标志，
// 不静默丢弃跳变信息，交由上层决定是否隔离。
func (u *Unwrapper) Extend(ts uint32) (extended uint64, backward bool) {
	if !u.initialized {
		u.initialized = true
		u.baseRaw = ts
		u.lastRaw = ts
		return uint64(int64(ts)), false
	}
	d := int64(ts) - int64(u.lastRaw)
	switch {
	case d < -u.half:
		d += u.modulus // 前向回绕
	case d > u.half:
		d -= u.modulus // 时间戳倒退
		backward = true
	}
	u.acc += d
	u.lastRaw = ts
	return uint64(int64(u.baseRaw) + u.acc), backward
}

// Reset 清空状态，供检查点续跑后重建窗口。
func (u *Unwrapper) Reset() {
	u.baseRaw = 0
	u.acc = 0
	u.lastRaw = 0
	u.initialized = false
}
