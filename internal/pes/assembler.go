package pes

// Assembler 按 PID 重组 PES 流。
//
// 每个 PUSI 包开启新 PES；后续负载追加直到声明长度满足。
// 重组期间任何 CC 缺口都会把当前 PES 标记为 Untrusted，并通过
// 返回的错误让上层把对应轨道置为不可信（状态沿 PES→时间线传播）。
type Assembler struct {
	states map[uint16]*asmState
}

type asmState struct {
	buf       []byte
	need      int  // 期望的完整 PES 字节数；0 表示未知
	untrusted bool
}

// NewAssembler 创建重组器。
func NewAssembler() *Assembler {
	return &Assembler{states: map[uint16]*asmState{}}
}

// Push 输入一个 PES PID 的 TS 包负载与连续性事件信息。
//
// pusi 为负载起始标记；gap 为本次相对上一包的 CC 缺口数；
// duplicate 为重复包（调用方应先丢弃，这里双保险直接忽略）。
// 返回 0 个或 1 个完成的 PES。
func (a *Assembler) Push(pid uint16, payload []byte, pusi bool, gap int, duplicate bool) (*Packet, error) {
	if duplicate || len(payload) == 0 {
		return nil, nil
	}
	st := a.states[pid]
	if st == nil {
		st = &asmState{}
		a.states[pid] = st
	}
	if gap > 0 && len(st.buf) > 0 {
		// 缺口落在 PES 中间：内容必然不完整，标记并继续收集到边界，
		// 同时把错误返回给上层传播不可信状态。
		st.untrusted = true
	}
	if pusi {
		if len(st.buf) > 0 {
			// 上一个 PES 未正常结束就遇到新起始：丢弃残片并记录截断。
			st.buf = nil
		}
		st.buf = append(st.buf, payload...)
		st.untrusted = gap > 0
	} else {
		if len(st.buf) == 0 {
			// 没有起始包的孤儿负载，忽略。
			return nil, nil
		}
		st.buf = append(st.buf, payload...)
	}
	// 硬上限优先于一切：实际缓冲超限立即拒绝，禁止后续截断绕过。
	if len(st.buf) > MaxPESLength {
		st.buf = nil
		return nil, ErrPESLength
	}
	// 再核对声明长度上限，避免畸形长度在数据到齐后放大占用。
	if len(st.buf) >= MinHeaderLen {
		declared := int(st.buf[4])<<8 | int(st.buf[5])
		if declared > MaxPESLength {
			st.buf = nil
			return nil, ErrPESLength
		}
	}
	// 达到声明长度后立即截断，丢弃同一 TS 包内的 0xFF 填充。
	if st.complete() && len(st.buf) > st.need {
		st.buf = st.buf[:st.need]
	}
	if !st.complete() {
		return nil, nil
	}
	pkt, _, err := parseHeader(st.buf)
	if pkt == nil {
		st.buf = nil
		return nil, err
	}
	pkt.Untrusted = st.untrusted
	finished := pkt
	st.buf = nil
	st.untrusted = false
	return finished, err
}

// complete 判断缓冲是否已包含完整 PES（依据 PES_packet_length）。
func (s *asmState) complete() bool {
	if len(s.buf) < MinHeaderLen {
		return false
	}
	declared := int(s.buf[4])<<8 | int(s.buf[5])
	if declared == 0 {
		// 长度 0 仅对视频流合法；本项目夹具始终写明确长度。
		return false
	}
	need := 6 + declared
	s.need = need
	return len(s.buf) >= need
}
// Flush 在流结束时取出未完成残片，标记 Truncated。
func (a *Assembler) Flush(pid uint16) *Packet {
	st := a.states[pid]
	if st == nil || len(st.buf) < MinHeaderLen {
		if st != nil {
			st.buf = nil
		}
		return nil
	}
	pkt, _, err := parseHeader(st.buf)
	if pkt == nil {
		st.buf = nil
		return nil
	}
	pkt.Truncated = err != nil
	pkt.Untrusted = pkt.Untrusted || st.untrusted
	st.buf = nil
	st.untrusted = false
	return pkt
}

// Reset 清空全部重组状态。
func (a *Assembler) Reset() {
	for pid := range a.states {
		delete(a.states, pid)
	}
}
