package rtptrace

import (
	"fmt"

	"relaycheck/internal/model"
)

// tsJumpTicks 判定时间戳跳变的阈值（RTP 时钟 ticks）。
// 90kHz 视频时钟下 5_000_000 ticks 约 55.6s，远超正常帧间隔。
const tsJumpTicks = 5_000_000

// detect 依据当前包与上一个包的扩展序号、时间戳登记故障。
func (tr *Trace) detect(segment, record int, ext uint64, ts uint32,
	prevExt uint64, havePrev bool, prevTS uint32, haveTS bool,
) {
	if !havePrev {
		return
	}
	prev16 := uint16(prevExt)
	cur16 := uint16(ext)

	switch {
	case cur16 == 0 && prev16 != 0 && ext > prevExt:
		// 16 位正向回绕（0xFFFF -> 0x0000）。
		tr.Faults = append(tr.Faults, model.Fault{
			Kind: model.FaultRTPWrap, Segment: segment, Record: record,
			Detail: fmt.Sprintf("sequence wrapped %d -> %d (ext %d)", prev16, cur16, ext),
		})
	case ext < prevExt:
		// 扩展序号倒退：乱序到达。
		tr.Faults = append(tr.Faults, model.Fault{
			Kind: model.FaultRTPReorder, Segment: segment, Record: record,
			Detail: fmt.Sprintf("out-of-order ext seq %d after %d", ext, prevExt),
		})
	case ext > prevExt+1:
		// 扩展序号跳跃：中间有丢包。
		missing := ext - prevExt - 1
		tr.Faults = append(tr.Faults, model.Fault{
			Kind: model.FaultRTPLoss, Segment: segment, Record: record,
			Detail: fmt.Sprintf("%d packet(s) missing between ext %d and %d", missing, prevExt, ext),
		})
	}

	// 时间戳跳变：按无符号 32 位回绕求差值，超过阈值即报警。
	if haveTS {
		delta := ts - prevTS
		if delta > tsJumpTicks || (ts < prevTS && (prevTS-ts) > tsJumpTicks) {
			tr.Faults = append(tr.Faults, model.Fault{
				Kind: model.FaultRTPTimestampJump, Segment: segment, Record: record,
				Detail: fmt.Sprintf("timestamp jump %d -> %d (delta %d)", prevTS, ts, delta),
			})
		}
	}
}

// MapToTimeline 把 RTP 时间戳（clockRate 时钟）换算到纳秒有理数时间轴，
// 返回首个包对应的纳秒时间，便于与分片 tfdt 时间轴对齐。
// clockRate<=0 时返回非法时间。
func MapToTimeline(rtpTS uint32, clockRate uint32) model.Rat {
	if clockRate == 0 {
		return model.InvalidRat
	}
	return model.FromTicks(int64(rtpTS), int64(clockRate))
}

// Report 汇总为模型层 RTPReport。
func (tr *Trace) Report() model.RTPReport {
	rep := model.RTPReport{Packets: len(tr.Packets), Faults: tr.Faults}
	if len(tr.Packets) > 0 {
		rep.MinSeq = tr.Packets[0].Seq16
		rep.MaxExtSeq = tr.Packets[len(tr.Packets)-1].ExtSeq
	}
	return rep
}
