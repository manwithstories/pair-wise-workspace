package main

import (
	"fmt"
	"math/big"
)

// unwrapState 是单个 (track, epoch) 的解绕状态。
// 设备重启必须显式切换 epoch：新 epoch 从独立状态重新归零。
type unwrapState struct {
	period  *big.Int // 2^bits
	half    *big.Int // 2^(bits-1)
	lastRaw int64    // 上一条原始计数器值
	lastUnw *big.Int // 上一条归零后的解绕值
	tb      Timebase
	started bool
}

// Unwrapper 按 (track, epoch) 维护解绕状态机。
type Unwrapper struct {
	states map[string]*unwrapState
	order  []string // 首次出现顺序，保证段号稳定可追溯
	tbs    map[string]Timebase
}

func uwKey(track string, epoch int64) string {
	return fmt.Sprintf("%s/%d", track, epoch)
}

func NewUnwrapper() *Unwrapper {
	return &Unwrapper{
		states: make(map[string]*unwrapState),
		tbs:    make(map[string]Timebase),
	}
}

// Keys 返回首次出现顺序的 (track, epoch) 键列表。
func (u *Unwrapper) Keys() []string {
	out := make([]string, len(u.order))
	copy(out, u.order)
	return out
}

// Timebase 返回某 (track, epoch) 解析出的时间基；未知键返回零值。
func (u *Unwrapper) Timebase(track string, epoch int64) Timebase {
	return u.tbs[uwKey(track, epoch)]
}

// Unwrap 对一条记录执行计数器解绕并返回“该轨该 epoch 首点归零”后的
// ticks。规则：相邻真实增量必须严格小于半周期；负跳变且回绕后增量
// 小于半周期视为回绕，其余倒退（含恰好半周期）一律报错。
func (u *Unwrapper) Unwrap(r *Record) (int64, error) {
	key := uwKey(r.Track, r.Epoch)
	st, ok := u.states[key]
	if !ok {
		period := new(big.Int).Lsh(bigOne, uint(r.Bits))
		st = &unwrapState{
			period:  period,
			half:    new(big.Int).Rsh(period, 1),
			lastRaw: r.PTS,
			lastUnw: new(big.Int),
			tb:      r.TB,
			started: true,
		}
		u.states[key] = st
		u.order = append(u.order, key)
		u.tbs[key] = r.TB
		return 0, nil
	}
	if st.tb != r.TB {
		return 0, lineError(r.Line,
			"timebase changed within track=%s epoch=%d: %d/%d -> %d/%d",
			r.Track, r.Epoch, st.tb.Num, st.tb.Den, r.TB.Num, r.TB.Den)
	}
	diff := r.PTS - st.lastRaw // 原始计数器差值，范围远小于 int64
	var delta *big.Int
	switch {
	case diff > 0:
		d := big.NewInt(diff)
		if d.Cmp(st.half) >= 0 {
			return 0, lineError(r.Line,
				"forward jump %d >= half period on track=%s epoch=%d (missing epoch switch?)",
				diff, r.Track, r.Epoch)
		}
		delta = d
	case diff < 0:
		// 负跳变：候选真实增量 = diff + period，必须严格小于半周期才算回绕。
		d := new(big.Int).Add(big.NewInt(diff), st.period)
		if d.Cmp(st.half) >= 0 {
			return 0, lineError(r.Line,
				"backward jump on track=%s epoch=%d not explained by wrap (delta after wrap=%s ticks)",
				r.Track, r.Epoch, d.String())
		}
		delta = d
	default:
		delta = big.NewInt(0) // 重复时间戳（量化后相同）允许通过
	}
	next := new(big.Int).Add(st.lastUnw, delta)
	if next.Sign() < 0 {
		return 0, lineError(r.Line, "unwrapped pts went negative on track=%s epoch=%d", r.Track, r.Epoch)
	}
	st.lastRaw = r.PTS
	st.lastUnw = next
	if !next.IsInt64() {
		return 0, lineError(r.Line, "unwrapped pts overflows int64 on track=%s epoch=%d", r.Track, r.Epoch)
	}
	return next.Int64(), nil
}
