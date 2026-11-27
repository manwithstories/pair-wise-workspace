package synth

import (
	"archivemedia/internal/packet"
)

// injectFaults 在基准事件流上确定性注入故障，输出按到达顺序排列的事件切片。
// 所有随机决策来自同一 rng，保证同 seed 可复现。
func injectFaults(truth []*packet.Event, rng rngSource, faults []Fault, st *FaultStats) []*packet.Event {
	enabled := map[Fault]bool{}
	for _, f := range faults {
		enabled[f] = true
	}
	n := len(truth)
	// alive 标记哪些基准序号仍实际到达（burst 会移除）。
	alive := make(map[int]bool, n)
	for i := range truth {
		alive[i] = true
	}

	// 1) 突发丢失：选一个中段区间整体移除。
	if enabled[FaultBurst] && n > 12 {
		start := 2 + rng.Intn(n/4)
		span := 2 + rng.Intn(3) // 2~4 个连续包
		if start+span >= n {
			span = n - start - 1
		}
		for i := start; i < start+span && i < n; i++ {
			alive[i] = false
			st.BurstMissing = append(st.BurstMissing, truth[i].Seq)
			st.Bursts++
		}
	}

	// 工作集：当前会到达的事件（深拷贝，避免改动 ground truth）。
	var work []*packet.Event
	for i, e := range truth {
		if alive[i] {
			work = append(work, cloneEvent(e))
		}
	}

	// 2) CRC 损坏：随机挑一个存活包破坏载荷（不改变键）。
	if enabled[FaultCRC] && len(work) > 2 {
		idx := rng.Intn(len(work))
		corruptPayload(work[idx])
		st.CRC++
	}

	// 3) 重复：复制一个存活包，副本到达序靠后，内容完全相同。
	if enabled[FaultDuplicate] && len(work) > 2 {
		idx := rng.Intn(len(work))
		dup := cloneEvent(work[idx])
		dup.ArrivalIndex = n + st.Duplicates
		dup.ArrivalMicros += 5_000
		insertAt := len(work)
		if insertAt > idx+2 {
			insertAt = idx + 2
		}
		work = insertEvent(work, insertAt, dup)
		st.Duplicates++
	}

	// 4) 乱序：交换若干对相邻事件的到达位置。
	if enabled[FaultReorder] && len(work) > 4 {
		swaps := 1 + rng.Intn(2)
		for k := 0; k < swaps; k++ {
			idx := 1 + rng.Intn(len(work)-2)
			work[idx], work[idx+1] = work[idx+1], work[idx]
			st.Reorders++
		}
	}

	// 5) 迟到：把一个事件的到达时间显著推后（位置也后移）。
	if enabled[FaultLate] && len(work) > 4 {
		idx := rng.Intn(len(work) - 1)
		late := work[idx]
		work = removeEvent(work, idx)
		late.ArrivalMicros += 800_000 // 远超正常 20ms 间隔
		work = append(work, late)
		st.Late++
	}

	// 重新分配到达序，反映最终到达顺序。
	for i, e := range work {
		e.ArrivalIndex = i
	}
	return work
}

// rngSource 抽象 rand.Rand 的用到的方法，便于测试替换。
type rngSource interface {
	Intn(n int) int
}

func corruptPayload(e *packet.Event) {
	if len(e.Payload) == 0 {
		return
	}
	// 最小确定性破坏载荷：单字节 0xFF。
	// 实测对任意长度的 Opus/AAC 裸包均被解码器 100% 拒绝（INVALIDDATA），
	// 事件仍通过结构校验（非空、时长合法），但解码时计入损坏并以静音占位，
	// 绝不伪装成有效音频；同 seed 下结果完全可复现。
	e.Payload = []byte{0xFF}
}

func cloneEvent(e *packet.Event) *packet.Event {
	cp := *e
	cp.Payload = append([]byte(nil), e.Payload...)
	return &cp
}

func insertEvent(es []*packet.Event, at int, e *packet.Event) []*packet.Event {
	if at >= len(es) {
		return append(es, e)
	}
	es = append(es, nil)
	copy(es[at+1:], es[at:])
	es[at] = e
	return es
}

func removeEvent(es []*packet.Event, idx int) []*packet.Event {
	return append(es[:idx], es[idx+1:]...)
}
