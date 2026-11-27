// Package clock 提供整数有理数时基换算与 PTS 回绕展开。
// 所有运算保持纯整数：禁止浮点进入时间计算，乘法溢出即返回错误而非静默截断。
package clock

import (
	"errors"
	"math/big"
)

// ErrOverflow 表示有理数换算超出 int64 可表示范围。
var ErrOverflow = errors.New("clock: rational conversion overflow")

// Rational 是分子/分母均为正整数的有理数（时间滴答比例）。
type Rational struct {
	Num uint64
	Den uint64
}

// MasterTimebase 是全流水线统一主时基：微秒。所有轨道最终归并到此时基。
const MasterTimebase uint64 = 1_000_000

// Convert 将 value 个 src 滴答换算为 dst 时基下的滴答（四舍五入）。
// 中间乘积用 big.Int 校验，一旦超出 int64 立即返回 ErrOverflow。
func Convert(value int64, src, dst uint64) (int64, error) {
	if src == 0 || dst == 0 {
		return 0, errors.New("clock: zero timebase")
	}
	num := new(big.Int).Mul(big.NewInt(value), big.NewInt(int64(dst)))
	num.Mul(num, big.NewInt(2)) // 为四舍五入预留
	den := big.NewInt(int64(src))
	num.Quo(num, den)
	num.Add(num, big.NewInt(1))
	num.Quo(num, big.NewInt(2))
	if !num.IsInt64() {
		return 0, ErrOverflow
	}
	return num.Int64(), nil
}

// ConvertU 是无符号版本，供回绕前的原始 uint64 滴答使用，返回主时基滴答。
func ConvertU(value uint64, src, dst uint64) (uint64, error) {
	if src == 0 || dst == 0 {
		return 0, errors.New("clock: zero timebase")
	}
	num := new(big.Int).Mul(new(big.Int).SetUint64(value), new(big.Int).SetUint64(dst))
	num.Mul(num, big.NewInt(2))
	num.Quo(num, new(big.Int).SetUint64(src))
	num.Add(num, big.NewInt(1))
	num.Quo(num, big.NewInt(2))
	if !num.IsUint64() {
		return 0, ErrOverflow
	}
	return num.Uint64(), nil
}

// Scale 按 r 缩放 value，同样做溢出检查。
func Scale(value int64, r Rational) (int64, error) {
	return Convert(value, r.Den, r.Num)
}
