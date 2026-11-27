package clock

import "errors"

// ErrWrapJump 表示回绕展开遇到无法解释的反向跳变（非回绕、非重复）。
var ErrWrapJump = errors.New("clock: unexplained backward PTS jump")

// Unwrapper 对单条轨道做有状态的 PTS 回绕展开。
// 输入是回绕域内的原始 PTS，输出单调不减的 64 位展开滴答。
type Unwrapper struct {
	modulus uint64
	last    uint64 // 上一个展开后的 PTS
	started bool
	wraps   int
}

// NewUnwrapper 以轨道回绕模数（必须为 2 的幂）构造展开器。
func NewUnwrapper(modulus uint64) (*Unwrapper, error) {
	if modulus == 0 || modulus&(modulus-1) != 0 {
		return nil, errors.New("clock: wrap modulus must be a positive power of two")
	}
	return &Unwrapper{modulus: modulus}, nil
}

// Unwrap 展开下一个原始 PTS。重复包（相同原始 PTS）原样返回当前值；
// 检测到回绕则累加模数并计数；异常反向跳变返回 ErrWrapJump。
func (u *Unwrapper) Unwrap(raw uint64) (uint64, error) {
	if !u.started {
		u.started = true
		u.last = raw
		return raw, nil
	}
	switch {
	case raw == u.last%u.modulus:
		// 重复包：不推进时钟。
		return u.last, nil
	case raw < u.last%u.modulus:
		// 正向流中出现变小：唯一合法解释是发生回绕。
		next := u.last + (u.modulus-u.last%u.modulus) + raw
		if next < u.last {
			return 0, ErrOverflow
		}
		u.wraps++
		u.last = next
		return next, nil
	default:
		delta := raw - u.last%u.modulus
		next := u.last + delta
		if next < u.last {
			return 0, ErrOverflow
		}
		u.last = next
		return next, nil
	}
}

// Wraps 返回至今处理的回绕次数。
func (u *Unwrapper) Wraps() int { return u.wraps }
