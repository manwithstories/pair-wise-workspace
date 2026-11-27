package timeline

import (
	"math/big"

	"bcast/internal/model"
)

// DriftEstimate 是对单条轨道时钟漂移的估计结果。
// 以期望时间（无漂移微秒）为自变量、实际归一化 PTS 为因变量做整数最小二乘，
// VFR 抖动作为零均值噪声被斜率平均掉，斜率换算为百万分率（ppm）。
type DriftEstimate struct {
	TrackID     uint8
	PPM         int64 // 漂移斜率（百万分率），正表示时钟偏快
	InterceptUS int64
	N           int
}

// EstimateDrift 对一条轨道的有序样本做纯整数 OLS 拟合。
// 自变量 x = 期望时间 = seq*标称周期（微秒）；因变量 y = 实际 PTS（微秒）。
// 全部中间量用 big.Int 计算，杜绝乘法溢出；最终斜率四舍五入到整数 ppm。
func EstimateDrift(t model.TrackPlan, samples []model.Sample) (DriftEstimate, bool) {
	n := len(samples)
	if n < 2 {
		return DriftEstimate{TrackID: t.ID}, false
	}
	var sx, sy, sxx, sxy big.Int
	bigN := big.NewInt(int64(n))
	for _, s := range samples {
		// 自变量用真实序号而非切片下标，丢包后仍对齐期望时间。
		x := big.NewInt(int64(s.Seq) * t.PeriodUS)
		y := big.NewInt(s.PTS)
		sx.Add(&sx, x)
		sy.Add(&sy, y)
		sxx.Add(&sxx, new(big.Int).Mul(x, x))
		sxy.Add(&sxy, new(big.Int).Mul(x, y))
	}
	// 斜率 m = (N*Σxy − Σx*Σy) / (N*Σxx − (Σx)^2)
	num := new(big.Int).Sub(
		new(big.Int).Mul(bigN, &sxy),
		new(big.Int).Mul(&sx, &sy),
	)
	den := new(big.Int).Sub(
		new(big.Int).Mul(bigN, &sxx),
		new(big.Int).Mul(&sx, &sx),
	)
	if den.Sign() == 0 {
		return DriftEstimate{TrackID: t.ID, N: n}, false
	}
	// ppm = (m − 1) * 1e6：y≈(1+ppm/1e6)·x，先在整数域减去基准斜率再换算，
	// 分子先乘 1e6 后做带符号四舍五入除法。
	ppmNum := new(big.Int).Sub(num, den)
	ppmNum.Mul(ppmNum, big.NewInt(1_000_000))
	ppm := roundedQuo(ppmNum, den)
	// 截距 b = (Σy − m*Σx)/N，用有理数 m=num/den 保持整数精度。
	interNum := new(big.Int).Sub(
		new(big.Int).Mul(&sy, den),
		new(big.Int).Mul(num, &sx),
	)
	interDen := new(big.Int).Mul(bigN, den)
	intercept := roundedQuo(interNum, interDen)
	return DriftEstimate{
		TrackID: t.ID, PPM: ppm.Int64(), InterceptUS: intercept.Int64(), N: n,
	}, true
}

// EstimateAll 对时间线内每条声明轨道估计漂移，按轨道 ID 有序返回。
func EstimateAll(tl *Timeline) []DriftEstimate {
	out := make([]DriftEstimate, 0, len(tl.Plan.Tracks))
	for _, t := range tl.Plan.Tracks {
		if est, ok := EstimateDrift(t, tl.ByTrack[t.ID]); ok {
			out = append(out, est)
		}
	}
	return out
}

// roundedQuo 计算 big.Int 带符号四舍五入除法 a/b（b 非零）。
func roundedQuo(a, b *big.Int) *big.Int {
	sign := int64(1)
	aa, bb := new(big.Int).Set(a), new(big.Int).Set(b)
	if aa.Sign() < 0 {
		sign = -1
		aa.Neg(aa)
	}
	if bb.Sign() < 0 {
		sign = -sign
		bb.Neg(bb)
	}
	q := new(big.Int).Quo(aa, bb)
	twoRem := new(big.Int).Mod(aa, bb)
	twoRem.Mul(twoRem, big.NewInt(2))
	if twoRem.Cmp(bb) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Mul(q, big.NewInt(sign))
}
