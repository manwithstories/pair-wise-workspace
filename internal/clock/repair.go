package clock

import (
	"math"
	"sort"
)

// RepairConfig 控制时间轴修复行为。所有阈值都必须显式配置，
// 不存在静默猜测路径：偏离模型超过 OutlierTick 的观测一律标记不可判定。
type RepairConfig struct {
	// Window 是节点线性漂移拟合的观测窗口（按片段序号计），至少为 2。
	Window int
	// OutlierTick 是拟合残差容忍度（90 kHz Tick），超过即标记不可判定。
	OutlierTick Tick
	// Expected 给出每个片段序号的标称起始时间；缺片序号不会出现在其中。
	Expected map[int]Tick
}

// Unwrap33 把一串 33 位回绕空间内的原始时间戳展开为单调不减的绝对 Tick。
//
// 依据相邻观测之间更短的弧判断是否跨越 2^33 边界；任何输入若超过
// 2^33-1 立即返回 ErrWrapOverflow，绝不截断。
func Unwrap33(raw []uint64) ([]Tick, int, error) {
	return unwrapMod(raw, WrapMod)
}

// Unwrap32 把一串 32 位（RTP 风格）时间戳展开为绝对 Tick，回绕模数为 2^32。
func Unwrap32(raw []uint32) ([]Tick, int, error) {
	expanded := make([]uint64, len(raw))
	for i, v := range raw {
		expanded[i] = uint64(v)
	}
	return unwrapMod(expanded, 1<<32)
}

// unwrapMod 是按指定模数展开回绕的通用实现，依据相邻观测间的更短弧判定。
func unwrapMod(raw []uint64, mod uint64) ([]Tick, int, error) {
	out := make([]Tick, len(raw))
	if len(raw) == 0 {
		return out, 0, nil
	}
	half := int64(mod / 2)
	var wraps int
	var acc int64
	prev := raw[0]
	if prev >= mod {
		return nil, 0, ErrWrapOverflow
	}
	out[0] = Tick(int64(prev))
	for i := 1; i < len(raw); i++ {
		cur := raw[i]
		if cur >= mod {
			return nil, 0, ErrWrapOverflow
		}
		delta := int64(cur) - int64(prev)
		if delta < -half {
			// cur 回绕到低值：实际前进了 mod + delta。
			acc += int64(mod)
			wraps++
		} else if delta > half {
			// 反向大跳，相当于回退一个 mod。
			acc -= int64(mod)
			wraps++
		}
		out[i] = Tick(acc + int64(cur))
		prev = cur
	}
	return out, wraps, nil
}

// FitDrift 在观测窗口上做最小二乘节点线性拟合：observed = slope*expected + b。
//
// 点数不足时返回斜率 1、截距 0 的中性模型，调用方据此无法做漂移修正，
// 但也不会得到伪造的漂移结论。
func FitDrift(obs []Observation, window int) DriftModel {
	m := DriftModel{Slope: 1, Window: window, Points: len(obs)}
	n := len(obs)
	if n < 2 {
		return m
	}
	if window > 0 && window < n {
		obs = obs[n-window:]
		n = len(obs)
		m.Points = n
	}
	var sx, sy, sxx, sxy float64
	for _, o := range obs {
		x := float64(o.Expected)
		y := float64(o.Observed)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	denom := float64(n)*sxx - sx*sx
	if denom == 0 {
		return m
	}
	m.Slope = (float64(n)*sxy - sx*sy) / denom
	m.Intercept = (sy - m.Slope*sx) / float64(n)
	m.DriftPPM = (m.Slope - 1) * 1_000_000
	return m
}

// FitDriftRobust 使用中位数法做稳健节点线性拟合：先取所有点对斜率的中位数，
// 再取 (y - slope*x) 的中位数作为截距。单个异常观测无法拉动整体模型，
// 从而保证异常点能被残差阈值正确识别而不是被模型吸收。
func FitDriftRobust(obs []Observation, window int) DriftModel {
	m := DriftModel{Slope: 1, Window: window, Points: len(obs)}
	n := len(obs)
	if n < 2 {
		return m
	}
	if window > 0 && window < n {
		obs = obs[n-window:]
		n = len(obs)
		m.Points = n
	}
	slopes := make([]float64, 0, n*(n-1)/2)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			dx := float64(obs[j].Expected - obs[i].Expected)
			if dx == 0 {
				continue
			}
			slopes = append(slopes, float64(obs[j].Observed-obs[i].Observed)/dx)
		}
	}
	if len(slopes) == 0 {
		return m
	}
	sort.Float64s(slopes)
	m.Slope = medianSorted(slopes)
	intercepts := make([]float64, n)
	for i, o := range obs {
		intercepts[i] = float64(o.Observed) - m.Slope*float64(o.Expected)
	}
	sort.Float64s(intercepts)
	m.Intercept = medianSorted(intercepts)
	m.DriftPPM = (m.Slope - 1) * 1_000_000
	return m
}

func medianSorted(s []float64) float64 {
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// Residual 返回观测点相对漂移模型的残差（Tick）。
func (m DriftModel) Residual(o Observation) Tick {
	pred := m.Slope*float64(o.Expected) + m.Intercept
	return Tick(math.Round(float64(o.Observed) - pred))
}

// repairOrder 按 (轨道, 片段序号, 片段内样本号) 排序，保证修复确定。
func repairOrder(s []Sample) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].TrackID != s[j].TrackID {
			return s[i].TrackID < s[j].TrackID
		}
		if s[i].Sequence != s[j].Sequence {
			return s[i].Sequence < s[j].Sequence
		}
		return s[i].SampleNumber < s[j].SampleNumber
	})
}

// RepairSamples 对一组携带原始 33 位时间戳的样本执行完整修复：
// 去回绕 → 漂移拟合/异常标记 → DTS 倒退与重复修复（保留 PTS-DTS 间隔）。
//
// presentSequences 是实际存在数据的片段序号集合；missing 是已知缺片序号，
// 缺片会在时间线上输出显式空洞而不是被静默跳过。
func RepairSamples(in []Sample, cfg RepairConfig, missing []int) (*Timeline, error) {
	if len(in) == 0 {
		return nil, ErrEmptyInput
	}
	if cfg.Window < 2 {
		cfg.Window = 2
	}
	samples := make([]Sample, len(in))
	copy(samples, in)
	repairOrder(samples)

	tl := &Timeline{Samples: samples}
	trackRaws := map[uint32][]uint64{}
	trackIdx := map[uint32][]int{}
	for i, s := range samples {
		trackRaws[s.TrackID] = append(trackRaws[s.TrackID], s.RawDTS)
		trackIdx[s.TrackID] = append(trackIdx[s.TrackID], i)
	}

	// 第一步：每路轨道独立去 33 位回绕。
	unwrapped := map[uint32][]Tick{}
	for track, raws := range trackRaws {
		abs, w, err := Unwrap33(raws)
		if err != nil {
			return nil, err
		}
		unwrapped[track] = abs
		if w > tl.WrapCount {
			tl.WrapCount = w
		}
	}

	// 第二步：用每片段首个视频观测（无视频则取首个轨道）拟合漂移。
	var fitTrack uint32
	for track := range unwrapped {
		fitTrack = track
		if samples[trackIdx[track][0]].IsVideo {
			break
		}
	}
	var obs []Observation
	firstOfSeq := map[int]Tick{}
	for _, i := range trackIdx[fitTrack] {
		seq := samples[i].Sequence
		if _, ok := firstOfSeq[seq]; ok {
			continue
		}
		firstOfSeq[seq] = unwrapped[fitTrack][indexOf(trackIdx[fitTrack], i)]
	}
	for seq, observed := range firstOfSeq {
		if exp, ok := cfg.Expected[seq]; ok {
			obs = append(obs, Observation{Sequence: seq, Expected: exp, Observed: observed})
		}
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].Sequence < obs[j].Sequence })
	tl.Drift = FitDriftRobust(obs, cfg.Window)

	// 第三步：残差超阈值的片段整段标记不可判定，不做静默猜测。
	badSeq := map[int]bool{}
	for _, o := range obs {
		r := tl.Drift.Residual(o)
		if r < 0 {
			r = -r
		}
		if r > cfg.OutlierTick {
			badSeq[o.Sequence] = true
		}
	}

	// 第四步：逐轨道修复倒退/重复 DTS，并保持 PTS-DTS 间隔。
	for track, idxs := range trackIdx {
		abs := unwrapped[track]
		var prevDTS Tick
		for k, i := range idxs {
			dts := abs[k]
			if k > 0 && dts < prevDTS {
				dts = prevDTS
				tl.Backwards++
			}
			if k > 0 && dts == prevDTS {
				dts = prevDTS + 1
				samples[i].IsDuplicate = true
				tl.Duplicate++
			}
			offset := Tick(int64(samples[i].RawPTS)) - Tick(int64(samples[i].RawDTS))
			samples[i].DTS = dts
			samples[i].PTS = dts + offset
			if badSeq[samples[i].Sequence] {
				samples[i].IsUndecidable = true
				tl.Undecidable++
			}
			prevDTS = dts
		}
	}

	// 第五步：缺片与不可判定片段输出显式时间线空洞。
	seqs := make([]int, 0, len(firstOfSeq))
	for seq := range firstOfSeq {
		seqs = append(seqs, seq)
	}
	sort.Ints(seqs)
	for _, seq := range missing {
		g := Gap{Sequence: seq, Missing: true}
		if a, b, ok := neighborBounds(seqs, seq, cfg.Expected); ok {
			g.Start, g.End = a, b
		}
		tl.Gaps = append(tl.Gaps, g)
	}
	for seq := range badSeq {
		g := Gap{Sequence: seq, Missing: false}
		if exp, ok := cfg.Expected[seq]; ok {
			g.Start = exp
			if next, ok := cfg.Expected[seq+1]; ok {
				g.End = next
			} else {
				g.End = exp
			}
		}
		tl.Gaps = append(tl.Gaps, g)
	}
	sort.Slice(tl.Gaps, func(i, j int) bool { return tl.Gaps[i].Sequence < tl.Gaps[j].Sequence })

	tl.MinDTS, tl.MaxDTS = samples[0].DTS, samples[0].DTS
	for _, s := range samples {
		if s.DTS < tl.MinDTS {
			tl.MinDTS = s.DTS
		}
		if s.DTS > tl.MaxDTS {
			tl.MaxDTS = s.DTS
		}
	}
	return tl, nil
}

func indexOf(haystack []int, needle int) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}
	return -1
}

func neighborBounds(present []int, seq int, expected map[int]Tick) (Tick, Tick, bool) {
	start, ok := expected[seq]
	if !ok {
		return 0, 0, false
	}
	if end, ok := expected[seq+1]; ok {
		return start, end, true
	}
	return start, start, true
}
