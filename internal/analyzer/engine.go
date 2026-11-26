package analyzer

import (
	"errors"
	"fmt"

	"tsproof/internal/adapter"
	"tsproof/internal/clock"
	"tsproof/internal/pes"
	"tsproof/internal/ts"
)

// unwrapPCR 展开 PCR 并统计回绕次数。
func (a *Analyzer) unwrapPCR(pid uint16, raw clock.Tick) {
	st := a.pcrUnwrap[pid]
	// 回绕边沿：上一原始字段在高位区、当前在低位区，且展开后跨越模数边界。
	if st.ok && st.lastRaw > clock.PCRModulus/2 && raw < clock.PCRModulus/2 {
		a.metrics.PCRWraps++
	}
	prev := clock.Unwrap(st.prev, raw, clock.PCRModulus, st.ok)
	a.pcrUnwrap[pid] = pcrState{prev: prev, lastRaw: raw, ok: true}
}

// handlePSI 装配并校验 PSI 节；只在完整且 CRC 正确的版本上切换映射。
func (a *Analyzer) handlePSI(p *ts.Packet) {
	asm, ok := a.sections[p.PID]
	if !ok {
		asm = ts.NewSectionAssembler()
		a.sections[p.PID] = asm
	}
	sections, err := asm.Push(p.Payload)
	if err != nil {
		a.metrics.CRCReject++
		a.errs = append(a.errs, fmt.Errorf("psi pid=0x%x: %w", p.PID, err))
		asm.Reset()
		return
	}
	for _, raw := range sections {
		sec, perr := parseAndCheck(raw)
		if perr != nil {
			// CRC 失败：记录可恢复错误，绝不切换映射。
			if errors.Is(perr, ts.ErrCRC) {
				a.metrics.CRCReject++
			}
			a.errs = append(a.errs, fmt.Errorf("psi pid=0x%x: %w", p.PID, perr))
			continue
		}
		a.applySection(sec)
	}
}

// parseAndCheck 隔离节解析，便于在测试中替换错误判定。
func parseAndCheck(raw []byte) (*ts.Section, error) {
	// 复用 ts 包内未导出的 parseSection 不可行，这里通过公开装配结果
	// 再校验：ts 包提供 ParseSectionRaw。
	return ts.ParseSection(raw)
}

// applySection 仅接受 current=true 的节，并在版本变化时切换映射。
func (a *Analyzer) applySection(sec *ts.Section) {
	if !sec.Current {
		return
	}
	switch sec.TableID {
	case ts.TableIDPAT:
		progs, err := ts.ParsePAT(sec)
		if err != nil {
			a.errs = append(a.errs, err)
			return
		}
		for _, pr := range progs {
			if a.current == nil || a.current.PMTPID != pr.PMTPID {
				a.ensureMap(sec.Version, pr.PMTPID)
				a.sections[pr.PMTPID] = ts.NewSectionAssembler()
			}
		}
	case ts.TableIDPMT:
		pmt, err := ts.ParsePMT(sec)
		if err != nil {
			a.errs = append(a.errs, err)
			return
		}
		next := buildMap(sec.Version, pmt)
		if a.current == nil || a.current.Version != sec.Version ||
			!mapsEqual(a.current, next) {
			if a.current != nil {
				a.metrics.PSISwitches++
			}
			a.current = next
		}
	}
}

func (a *Analyzer) ensureMap(version uint8, pmtPID uint16) {
	a.current = &StreamMap{
		Version:       version,
		PMTPID:        pmtPID,
		StreamTypePID: map[uint8]uint16{},
		PIDType:       map[uint16]uint8{},
	}
}

func buildMap(version uint8, pmt *ts.PMT) *StreamMap {
	m := &StreamMap{
		Version:       version,
		PMTPID:        0,
		PCRPID:        pmt.PCRPID,
		StreamTypePID: map[uint8]uint16{},
		PIDType:       map[uint16]uint8{},
	}
	for _, st := range pmt.Streams {
		m.StreamTypePID[st.StreamType] = st.PID
		m.PIDType[st.PID] = st.StreamType
	}
	return m
}

func mapsEqual(x, y *StreamMap) bool {
	if x.PCRPID != y.PCRPID || len(x.PIDType) != len(y.PIDType) {
		return false
	}
	for pid, ty := range x.PIDType {
		if y.PIDType[pid] != ty {
			return false
		}
	}
	return true
}

// handlePES 重组 PES，完成后交适配器解码并入时间线。
func (a *Analyzer) handlePES(p *ts.Packet, ev ts.CCEvent) {
	pkt, err := a.pesAsm.Push(p.PID, p.Payload, p.PUSI, ev.Gap, ev.Duplicate)
	if err != nil && !errors.Is(err, pes.ErrGap) {
		a.errs = append(a.errs, fmt.Errorf("pes pid=0x%x: %w", p.PID, err))
	}
	if pkt == nil {
		return
	}
	if pkt.Truncated {
		a.metrics.TruncatedPES++
	}
	a.decodeBlock(p.PID, pkt)
}

func (a *Analyzer) flushPES() {
	if a.current == nil {
		return
	}
	for pid := range a.current.PIDType {
		pkt := a.pesAsm.Flush(pid)
		if pkt != nil {
			if pkt.Truncated {
				a.metrics.TruncatedPES++
			}
			a.decodeBlock(pid, pkt)
		}
	}
}

func (a *Analyzer) decodeBlock(pid uint16, pkt *pes.Packet) {
	streamType, ok := a.current.PIDType[pid]
	if !ok {
		return
	}
	ad, err := a.registry.Lookup(streamType)
	if err != nil {
		a.metrics.UnknownTypes++
		a.errs = append(a.errs, err)
		return
	}
	idx := a.blockIdx[pid]
	// PTS 展开：PES PTS 是 90kHz，先转 27MHz 再按 2^33 展开。
	pts27 := int64(-1)
	if pkt.PTS90k >= 0 {
		raw27 := clock.PTSFrom90k(clock.Tick(pkt.PTS90k))
		st := a.ptsUnwrap[pid]
		pts27 = int64(clock.Unwrap(st.prev, raw27, clock.PTSModulus, st.ok))
		a.ptsUnwrap[pid] = ptsState{prev: clock.Tick(pts27), ok: true}
	}
	blk, derr := ad.Decode(pid, idx, pts27, pkt.Data)
	if derr != nil {
		a.metrics.AdapterErrors++
		a.errs = append(a.errs, fmt.Errorf("decode pid=0x%x: %w", pid, derr))
		return
	}
	blk.Untrusted = pkt.Untrusted
	a.blockIdx[pid] = idx + 1
	a.blocks[pid] = append(a.blocks[pid], blk)
	a.addToTimeline(blk)
}

func (a *Analyzer) addToTimeline(blk *adapter.Block) {
	tr := a.timeline.EnsureTrack(blk.TrackID)
	if blk.Untrusted {
		tr.MarkUntrust()
		a.metrics.Untrusted++
	}
	kind := clock.FrameInter
	if blk.Key {
		kind = clock.FrameKey
	}
	tr.Append(clock.TimelineEvent{
		TrackID: blk.TrackID,
		Index:   blk.Index,
		At:      clock.Tick(blk.PTS27),
		RawPTS:  clock.Tick(blk.PTS27),
		Kind:    kind,
	})
}

func (a *Analyzer) markTrackUntrusted(pid uint16) {
	if a.current != nil {
		if _, known := a.current.PIDType[pid]; known {
			a.timeline.EnsureTrack(pid).MarkUntrust()
		}
	}
}
