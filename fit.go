package main

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
)

// ErrUnreliable 表示锚点数量/跨度/内点不足以支撑可靠拟合。
var ErrUnreliable = errors.New("fit not reliable")

// Pair 是按同步编号配对的一组锚点，单位均为纳秒（精确 ticks 换算而来）。
type Pair struct {
	SyncID string
	LineV  int64
	LineA  int64
	Video  int64 // v：视频锚点解绕后的纳秒读数（参考钟）
	Audio  int64 // a：音频锚点解绕后的纳秒读数
}

// Fit 是单段（epoch）拟合结果：a = alpha*v + beta。
type Fit struct {
	Epoch    int64
	Alpha    *big.Rat
	Beta     *big.Rat // 纳秒：视频零点对应的音频读数
	PPM      float64  // (alpha-1)*1e6
	ResP95NS int64    // 内点残差 P95（纳秒）
	Pairs    int
	Inliers  int
}

// 锚点段门槛与异常点剔除阈值（规格硬性要求）。
const (
	minPairCount   = 6
	minSpanSeconds = 60.0
	rejectNS       = 2_000_000.0 // 残差超过 2ms 的锚点剔除
)

// FitSegment 对一个 epoch 的锚点对执行稳健拟合：
//  1. 成对斜率中位数（Theil–Sen）初始化斜率，截距取残差中位数；
//  2. 剔除 |a - (αv+β)| > 2ms 的点；
//  3. 对内点做最小二乘精化，并计算残差 P95。
//
// 数据量大时 Theil–Sen 采用确定性等步长抽样（固定上限，无随机源），
// 保证结果可复现；最小二乘本身为精确的 O(n) 闭式解。
func FitSegment(epoch int64, pairs []Pair) (*Fit, error) {
	if len(pairs) < minPairCount {
		return nil, fmt.Errorf("%w: epoch=%d has %d anchor pairs, need >= %d",
			ErrUnreliable, epoch, len(pairs), minPairCount)
	}
	xs := make([]float64, len(pairs))
	ys := make([]float64, len(pairs))
	var minV, maxV int64 = pairs[0].Video, pairs[0].Video
	for i, p := range pairs {
		xs[i] = float64(p.Video)
		ys[i] = float64(p.Audio)
		if p.Video < minV {
			minV = p.Video
		}
		if p.Video > maxV {
			maxV = p.Video
		}
	}
	if float64(maxV-minV)/1e9 < minSpanSeconds {
		return nil, fmt.Errorf("%w: epoch=%d anchor span %.3fs < %gs",
			ErrUnreliable, epoch, float64(maxV-minV)/1e9, minSpanSeconds)
	}

	// 1a. Theil–Sen 中位斜率（跳过视频读数相同的点对）。
	slopes := theilSenSlopes(xs, ys)
	if len(slopes) == 0 {
		return nil, fmt.Errorf("%w: epoch=%d all anchor video timestamps identical", ErrUnreliable, epoch)
	}
	alpha0 := medianF(slopes)
	if !finitePositive(alpha0) {
		return nil, fmt.Errorf("%w: epoch=%d degenerate median slope %v", ErrUnreliable, epoch, alpha0)
	}

	// 1b. 截距初值 = 残差中位数。
	res0 := make([]float64, len(pairs))
	for i := range pairs {
		res0[i] = ys[i] - alpha0*xs[i]
	}
	beta0 := medianF(res0)

	// 2. 2ms 硬阈值剔除异常锚点。
	var inX, inY []float64
	residAbs := make([]float64, 0, len(pairs))
	for i := range pairs {
		resid := ys[i] - (alpha0*xs[i] + beta0)
		if math.Abs(resid) <= rejectNS {
			inX = append(inX, xs[i])
			inY = append(inY, ys[i])
			residAbs = append(residAbs, math.Abs(resid))
		}
	}
	if len(inX) < minPairCount {
		return nil, fmt.Errorf("%w: epoch=%d only %d inliers after 2ms rejection, need >= %d",
			ErrUnreliable, epoch, len(inX), minPairCount)
	}

	// 3. 最小二乘精化（闭式解）。
	alpha, beta := leastSquares(inX, inY)
	if !finitePositive(alpha) || math.Abs(alpha-1.0) > 0.1 { // 漂移 >10% 视为拟合失败
		return nil, fmt.Errorf("%w: epoch=%d implausible slope alpha=%v", ErrUnreliable, epoch, alpha)
	}
	finalRes := make([]float64, len(inX))
	for i := range inX {
		finalRes[i] = math.Abs(inY[i] - (alpha*inX[i] + beta))
	}
	p95 := percentile(finalRes, 0.95)

	return &Fit{
		Epoch:    epoch,
		Alpha:    big.NewRat(1, 1).SetFloat64(alpha),
		Beta:     big.NewRat(1, 1).SetFloat64(beta),
		PPM:      (alpha - 1.0) * 1e6,
		ResP95NS: roundEven(p95),
		Pairs:    len(pairs),
		Inliers:  len(inX),
	}, nil
}

// theilSenSlopes 计算成对斜率，最多 theilCap 个；超限时按确定性等步长
// 抽取点对下标，不引入随机性。
func theilSenSlopes(xs, ys []float64) []float64 {
	const theilCap = 50000
	n := len(xs)
	total := n * (n - 1) / 2
	out := make([]float64, 0, minInt(total, theilCap)+1)
	step := 1
	if total > theilCap {
		step = (total + theilCap - 1) / theilCap
	}
	k := 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if k%step == 0 {
				dx := xs[j] - xs[i]
				if dx != 0 {
					out = append(out, (ys[j]-ys[i])/dx)
				}
			}
			k++
		}
	}
	return out
}

// leastSquares 普通最小二乘，返回斜率与截距。
func leastSquares(xs, ys []float64) (float64, float64) {
	n := float64(len(xs))
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return math.NaN(), 0
	}
	alpha := (n*sxy - sx*sy) / den
	beta := (sy - alpha*sx) / n
	return alpha, beta
}

// medianF / percentile 均采用最近秩（nearest-rank）定义，输入会被排序。
func medianF(v []float64) float64 { return percentile(v, 0.5) }

func percentile(v []float64, q float64) float64 {
	sorted := make([]float64, len(v))
	copy(sorted, v)
	sort.Float64s(sorted)
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func finitePositive(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 }

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// roundEven 用于统计量的展示级四舍六入五成双；逐项时间戳换算另有
// ratToInt64Even 保证精确。
func roundEven(v float64) int64 {
	return int64(math.RoundToEven(v))
}
