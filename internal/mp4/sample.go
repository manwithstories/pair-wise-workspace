package mp4

import (
	"fmt"

	"relaycheck/internal/model"
)

// 样本标志中 sample_depends_on 位于低 4 字节的最高 2 位；
// 取值 2（0x02000000）表示该样本不依赖其它样本，即关键帧。
const sampleDependsOnMask uint32 = 0x03000000
const sampleDependsOnNone uint32 = 0x02000000

const (
	trunDataOffset       = 0x000001
	trunFirstSampleFlags = 0x000004
	trunSampleDuration   = 0x000100
	trunSampleSize       = 0x000200
	trunSampleFlags      = 0x000400
	trunCTO              = 0x000800
)

// buildSamples 依据 tfdt 基准时间与 trun 条目还原每个样本的时间、大小与关键帧标志，
// 并校验样本声明总长度是否落在 mdat 负载范围内。
func buildSamples(rep *model.FragmentReport, seq int, md *moofData, timeScale uint32, moofOff int64, mdat *boxInfo) {
	sampleIdx := 0
	curTicks := int64(md.baseDecodeTime)
	var mdatPayloadLen int64
	var mdatPayloadOff int64
	if mdat != nil {
		mdatPayloadLen = int64(mdat.size) - int64(mdat.header)
		mdatPayloadOff = mdat.off + int64(mdat.header)
	}
	var declaredTotal int64

	for ti := range md.truns {
		trun := &md.truns[ti]
		flags := trun.GetFlags()
		var runCursor int64 // 相对 mdat 负载起点的样本写入位置
		if flags&trunDataOffset != 0 {
			// trun.DataOffset 相对 moof 起点，换算为相对 mdat 负载起点。
			runCursor = moofOff + int64(trun.DataOffset) - mdatPayloadOff
		}
		for i := uint32(0); i < trun.SampleCount; i++ {
			e := trun.Entries[i]
			dur := md.defaultDuration
			size := md.defaultSize
			sflags := md.defaultFlags
			haveFlags := md.haveDefaultFlags
			if flags&trunSampleDuration != 0 {
				dur = e.SampleDuration
			}
			if flags&trunSampleSize != 0 {
				size = e.SampleSize
			} else if !md.haveDefaultSize {
				size = 0
			}
			switch {
			case flags&trunSampleFlags != 0 && i > 0:
				sflags = e.SampleFlags
				haveFlags = true
			case flags&trunFirstSampleFlags != 0 && i == 0:
				sflags = trun.FirstSampleFlags
				haveFlags = true
			case flags&trunSampleFlags != 0:
				sflags = e.SampleFlags
				haveFlags = true
			}
			pts := model.FromTicks(curTicks, int64(timeScale))
			isSync := haveFlags && (sflags&sampleDependsOnMask) == sampleDependsOnNone
			rep.Samples = append(rep.Samples, model.SampleInfo{
				Index: sampleIdx, IsSync: isSync, Pts: pts, Size: int64(size),
			})
			if isSync {
				rep.SyncTimes = append(rep.SyncTimes, pts)
			}
			curTicks += int64(dur)
			declaredTotal += int64(size)
			if mdat != nil && runCursor+int64(size) > mdatPayloadLen {
				rep.Faults = append(rep.Faults, model.Fault{
					Kind: model.FaultSampleOverflow, Segment: seq, Sample: sampleIdx,
					Offset: mdatPayloadOff + runCursor,
					Detail: fmt.Sprintf("sample ends beyond mdat payload (need %d, have %d)",
						runCursor+int64(size), mdatPayloadLen),
				})
				if rep.FirstBadOff < 0 {
					rep.FirstBadOff = mdatPayloadOff + runCursor
				}
			}
			runCursor += int64(size)
			sampleIdx++
		}
	}
	if mdat != nil {
		rep.BytesInMDAT = mdatPayloadLen
		if declaredTotal > mdatPayloadLen {
			// 兜底：总样本长度超界（逐样本未覆盖到的累计情形）。
			rep.Faults = append(rep.Faults, model.Fault{
				Kind: model.FaultSampleOverflow, Segment: seq,
				Offset: mdatPayloadOff + mdatPayloadLen,
				Detail: fmt.Sprintf("declared sample bytes %d > mdat payload %d",
					declaredTotal, mdatPayloadLen),
			})
			if rep.FirstBadOff < 0 {
				rep.FirstBadOff = mdatPayloadOff
			}
		}
	}
}

// KeyframeGaps 返回相邻关键帧之间的时长（纳秒有理数）。
func KeyframeGaps(syncTimes []model.Rat) []model.Rat {
	if len(syncTimes) < 2 {
		return nil
	}
	gaps := make([]model.Rat, 0, len(syncTimes)-1)
	for i := 1; i < len(syncTimes); i++ {
		gaps = append(gaps, syncTimes[i].Sub(syncTimes[i-1]))
	}
	return gaps
}
