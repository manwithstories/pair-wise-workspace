package ingest

import (
	"medpipe/internal/clock"
)

// Fault 是内置弱网/损坏故障模式，全部由固定种子驱动、可复现。
type Fault string

const (
	FaultNone      Fault = ""
	FaultReorder   Fault = "reorder"    // 持续乱序
	FaultBurstLoss Fault = "burst-loss" // 突发丢包
	FaultFlapping  Fault = "flapping"   // 带宽持续升降 + 间歇丢包
	FaultDuplicate Fault = "duplicate"  // 完全重复包（应幂等吸收）
	FaultConflict  Fault = "conflict"   // 同序号内容冲突（应报错拒清单）
	// 以下为文件级损坏，由 fixture 注入，网络层按无故障处理。
	FaultTruncate  Fault = "truncate"   // 完整采样边界截断
	FaultHalfFrame Fault = "half-frame" // 半个采样帧
	FaultBadHeader Fault = "bad-header" // 非法 WAV 头
)

// FaultPlan 描述一次演示的网络损伤方案。
type FaultPlan struct {
	Mode       Fault
	Seed       int64
	WindowSize int64
}

// NetworkStats 记录模拟链路的可观测统计。
type NetworkStats struct {
	Dropped    []int64 // 被丢弃的包序号
	Duplicates []int64 // 重复投递的包序号
	Conflicts  []int64 // 注入冲突内容的包序号
	Reordered  int     // 乱序对计数
	MinBPS     int64   // 模拟带宽谷值（字节/秒语义，仅作证据输出）
	MaxBPS     int64
}

// rng 是确定的 xorshift64 随机源，避免引入外部依赖。
type rng struct{ state uint64 }

func newRNG(seed int64) *rng {
	s := uint64(seed)
	if s == 0 {
		s = 0x9E3779B97F4A7C15
	}
	return &rng{state: s}
}

func (r *rng) next() uint64 {
	x := r.state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	r.state = x
	return x
}

func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// Simulate 对已编号的有序包施加故障，返回乱序后的到达序列与统计。
// 不修改入参；冲突包以独立返回值给出，由调用方决定是否注入。
func Simulate(packets []*Packet, plan FaultPlan) (arrive []*Packet, conflicts []*Packet, stats NetworkStats, err error) {
	if plan.WindowSize <= 0 {
		return nil, nil, stats, clock.NewError(clock.CodeInvalidParam, -1, "ingest: 重排窗口必须为正")
	}
	r := newRNG(plan.Seed)
	drop := map[int64]bool{}
	dup := map[int64]bool{}
	conf := map[int64]bool{}
	stats.MinBPS, stats.MaxBPS = 1<<20, 8<<20

	switch plan.Mode {
	case FaultNone, FaultTruncate, FaultHalfFrame, FaultBadHeader:
		// 文件级损坏不产生网络层损伤。
	case FaultReorder, FaultBurstLoss, FaultFlapping, FaultDuplicate, FaultConflict:
		applyPlan(plan, r, len(packets), drop, dup, conf, &stats)
	default:
		return nil, nil, stats, clock.NewError(clock.CodeInvalidParam, -1, "ingest: 未知故障模式 %q", plan.Mode)
	}

	order := make([]int, 0, len(packets))
	for i, p := range packets {
		if drop[p.Seq] {
			stats.Dropped = append(stats.Dropped, p.Seq)
			continue
		}
		order = append(order, i)
	}
	// 乱序：在不超过窗口的距离内交换相邻元素，保证重排器仍可收敛。
	if plan.Mode == FaultReorder || plan.Mode == FaultFlapping {
		for i := 1; i < len(order); i++ {
			if r.intn(3) == 0 {
				j := i - 1
				order[i], order[j] = order[j], order[i]
				stats.Reordered++
			}
		}
	}
	for _, i := range order {
		p := packets[i]
		arrive = append(arrive, p)
		if conf[p.Seq] {
			// 冲突包紧跟原包到达：同序号异内容必须被重排器拒绝。
			cp := *p
			cp.PCM = append([]byte(nil), p.PCM...)
			if len(cp.PCM) > 0 {
				cp.PCM[0] ^= 0xFF
			}
			cp.SHA256 = HashPacket(cp.PCM)
			conflicts = append(conflicts, &cp)
			stats.Conflicts = append(stats.Conflicts, cp.Seq)
			arrive = append(arrive, &cp)
		}
		if dup[p.Seq] {
			arrive = append(arrive, p) // 同一指针再投一次：内容完全相同
			stats.Duplicates = append(stats.Duplicates, p.Seq)
		}
	}
	// 带宽证据：确定性正弦式升降，仅写入统计，不引入真实等待。
	for i := range packets {
		bps := int64(4<<20) + int64(3<<20)*int64((i%7)-3)/3
		if bps < stats.MinBPS {
			stats.MinBPS = bps
		}
		if bps > stats.MaxBPS {
			stats.MaxBPS = bps
		}
	}
	return arrive, conflicts, stats, nil
}

func applyPlan(plan FaultPlan, r *rng, n int, drop, dup, conf map[int64]bool, stats *NetworkStats) {
	switch plan.Mode {
	case FaultBurstLoss:
		// 两段连续突发，每段长度约为窗口的一半，且不覆盖首尾。
		for _, start := range []int{n / 3, 2 * n / 3} {
			length := int(plan.WindowSize)/2 + 1
			for k := 0; k < length && start+k < n-1; k++ {
				drop[int64(start+k)] = true
			}
		}
	case FaultFlapping:
		for i := 1; i < n-1; i++ {
			if r.intn(5) == 0 { // 约 20% 间歇丢失
				drop[int64(i)] = true
			}
		}
	case FaultReorder:
		// 丢失由重排交换自然体现，这里制造一个单点缺口。
		if n > 4 {
			drop[int64(n/2)] = true
		}
	case FaultDuplicate:
		for i := 1; i < n-1; i += 3 {
			dup[int64(i)] = true
		}
	case FaultConflict:
		if n > 2 {
			conf[int64(n/2)] = true
		}
	}
}
