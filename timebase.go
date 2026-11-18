package main

import (
	"errors"
	"math"
	"math/big"
)

// ErrOverflow 表示精确换算结果超出 int64 可表示范围。
// 两条导出路径共用同一套换算函数，因此对同一项必然给出相同结果。
var ErrOverflow = errors.New("rounded value overflows int64")

// Timebase 是有理数时间基：1 个 tick = Num/Den 秒。
type Timebase struct {
	Num int64
	Den int64
}

// Valid 要求分子分母均为正整数；零值与负值属于非法时间基。
func (tb Timebase) Valid() bool {
	return tb.Num > 0 && tb.Den > 0
}

var (
	bigOne    = big.NewInt(1)
	bigTwo    = big.NewInt(2)
	bigI64Max = big.NewInt(math.MaxInt64)
	bigI64Min = big.NewInt(math.MinInt64)
	bigGiga   = big.NewInt(1_000_000_000)
)

// ticksToRat 精确计算 ticks * num / den（秒），永不丢失精度。
func ticksToRat(ticks, num, den int64) *big.Rat {
	r := new(big.Rat).SetInt64(ticks)
	return r.Mul(r, big.NewRat(num, den))
}

// ticksToNs 把某时间基下的 ticks 精确换算为纳秒
// （ticks * num/den * 1e9）并按最近偶数舍入。
func ticksToNs(ticks, num, den int64) (int64, error) {
	sec := ticksToRat(ticks, num, den)
	ns := new(big.Rat).Mul(sec, new(big.Rat).SetInt(bigGiga))
	return ratToInt64Even(ns)
}

// secondsToTargetTicks 把秒值 r 换算到目标时间基 (tnum/tden 秒每 tick)：
// ticks = r * tden / tnum，最近偶数舍入，并检测 int64 溢出。
func secondsToTargetTicks(r *big.Rat, tnum, tden int64) (int64, error) {
	v := new(big.Rat).Mul(r, new(big.Rat).SetInt64(tden))
	v.Quo(v, new(big.Rat).SetInt64(tnum))
	return ratToInt64Even(v)
}

// ratToInt64Even 把有理数舍入为最近整数，等距情形向偶数靠拢（banker's
// rounding / round half to even），正确覆盖负值；结果越界返回 ErrOverflow。
func ratToInt64Even(r *big.Rat) (int64, error) {
	num, den := r.Num(), r.Denom()                        // den 恒正，符号由 num 携带
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int)) // 截断商 + 同号余数
	absRem := new(big.Int).Abs(rem)
	cmp := new(big.Int).Mul(bigTwo, absRem).Cmp(den) // 2*|rem| 与 den 比较
	if cmp > 0 || (cmp == 0 && q.Bit(0) == 1) {
		// 严格过半则向远离零方向走一步；恰好半偶且 q 为奇数时走到偶数。
		if num.Sign() < 0 {
			q.Sub(q, bigOne)
		} else {
			q.Add(q, bigOne)
		}
	}
	if q.Cmp(bigI64Max) > 0 || q.Cmp(bigI64Min) < 0 {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}
