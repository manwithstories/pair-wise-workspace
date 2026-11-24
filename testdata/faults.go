package testdata

import (
	"mediatime/internal/clock"
)

// driftPPM 是 wrap-drift-vfr 场景注入的设备时钟漂移（验收要求 150..250）。
const driftPPM = 200

// buildWrapDriftVFR 构造同时跨越 33 位回绕、带 200ppm 漂移、并含 VFR
// 重复时间戳的场景。
func (g *Generator) buildWrapDriftVFR(sc *Scenario, n int) {
	// 起点贴近 2^33 边界，使约第 6 片附近发生一次回绕。
	base := clock.Tick(clock.WrapMod) - 6*nominalDurationTick
	for seq := 0; seq < n; seq++ {
		spec := FragmentSpec{Sequence: seq, URI: fmtURI(seq)}
		for k := 0; k < samplesPerFragment; k++ {
			nominal := int64(base) + int64(seq)*nominalDurationTick + int64(k)*targetFrameTick
			// 设备时钟漂移：相对起点的位移按 (1+200ppm) 放大。
			drifted := int64(float64(nominal-int64(base)) * (1 + driftPPM/1_000_000.0))
			abs := int64(base) + drifted
			dts := uint64(abs) % clock.WrapMod
			// 在第 5 片注入一个 VFR 重复 DTS（与上一帧相同）。
			if seq == 5 && k == 7 {
				dts = spec.Samples[k-1].DTS
			}
			size := uint32(3333 + g.rng.Intn(134) - 67)
			spec.Samples = append(spec.Samples, SampleSpec{
				DTS: dts, PTS: dts + 900, Size: size, IsSync: k == 0,
			})
		}
		sc.FragmentSpec = append(sc.FragmentSpec, spec)
	}
}

// buildMissing 生成缺片场景：第 8、9 个序号不产生分片，
// 但清单仍保留其占位（上层据此输出显式时间线空洞）。
func (g *Generator) buildMissing(sc *Scenario, n int) {
	missing := map[int]bool{8: true, 9: true}
	for seq := 0; seq < n; seq++ {
		if missing[seq] {
			continue
		}
		base := uint64(seq) * nominalDurationTick
		spec := FragmentSpec{Sequence: seq, URI: fmtURI(seq)}
		for k := 0; k < samplesPerFragment; k++ {
			dts := base + uint64(k)*targetFrameTick
			size := uint32(3333 + g.rng.Intn(134) - 67)
			spec.Samples = append(spec.Samples, SampleSpec{
				DTS: dts, PTS: dts + 900, Size: size, IsSync: k == 0,
			})
		}
		sc.FragmentSpec = append(sc.FragmentSpec, spec)
	}
}

// buildTruncated 在第 3 片注入截断，制造 truncated_box 错误类别。
func (g *Generator) buildTruncated(sc *Scenario, n int) {
	g.buildNormal(sc, n)
	// 截断到 moof 内部，保证盒声明越过输入末尾。
	sc.FragmentSpec[3].TruncateAt = 12
}

// buildConflict 生成正常分片，并由 renderHLS(conflict=true) 注入冲突序号。
func (g *Generator) buildConflict(sc *Scenario, n int) {
	g.buildNormal(sc, n)
}

func fmtURI(seq int) string {
	return "seg" + itoa(seq) + ".m4s"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
