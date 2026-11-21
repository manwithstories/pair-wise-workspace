package inspect

import (
	"strconv"

	"tsbatch/internal/clock"
	"tsbatch/internal/fault"
	"tsbatch/internal/ts"
)

// Analyzer consumes ordered PacketEvents.
type Analyzer struct {
	MaxSegmentPackets uint64

	sink      Sink
	cc        *ts.CCTracker
	pcrTL     clock.Timeline
	ptsTL     clock.Timeline
	psi       map[uint16]*ts.Assembler
	tables    map[uint16]*ts.TableTracker
	pmtPIDs   map[uint16]bool
	mediaPIDs map[uint16]bool
	programs  []Program
	rep       Report

	segOpen   bool
	segID     int
	segStart  uint64
	segCount  uint64
	lastTime  int64
	damaged   bool
	dmgStart  uint64
	dmgReason fault.Category
}

func NewAnalyzer(s Sink, maxSegmentPackets uint64) *Analyzer {
	return &Analyzer{
		MaxSegmentPackets: maxSegmentPackets,
		sink:              s, cc: ts.NewCCTracker(),
		psi: map[uint16]*ts.Assembler{}, tables: map[uint16]*ts.TableTracker{},
		pmtPIDs: map[uint16]bool{}, mediaPIDs: map[uint16]bool{},
	}
}

func (a *Analyzer) mediaTime() int64 {
	if t, ok := a.pcrTL.Last(); ok {
		return t
	}
	return fault.UnknownTime
}

func (a *Analyzer) addFault(c fault.Category, pid uint16, idx uint64, detail string) {
	a.rep.Faults = append(a.rep.Faults, fault.Fault{
		Category: c, PID: pid, PacketIndex: idx,
		MediaTime: a.mediaTime(), Recoverable: true, Detail: detail,
	})
}

// Consume processes one packet in input order.
func (a *Analyzer) Consume(ev PacketEvent) {
	h := ev.Header
	if h.HasPCR {
		e := a.pcrTL.Unwrap(h.PCR)
		if e.Backward {
			a.addFault(fault.CatTimeJump, h.PID, ev.Index, "pcr moved backwards")
		}
	}
	damaged := false
	if h.TEI {
		a.addFault(fault.CatTEI, h.PID, ev.Index, "transport error indicator")
		damaged = true
	}
	switch v, gap := a.cc.Check(h.PID, h.CC, h.HasPayload); v {
	case ts.CCGap:
		a.addFault(fault.CatCCGap, h.PID, ev.Index, ccGapDetail(gap))
		damaged = true
	}
	if h.PID == 0 || a.pmtPIDs[h.PID] {
		a.consumePSI(ev)
		return // PSI packets are not segment media
	}
	if h.PID == 0x1FFF {
		return // null packets are dropped from the report stream
	}
	if ev.HasPTS {
		e := a.ptsTL.Unwrap(ev.PTSRaw)
		if e.Backward {
			a.addFault(fault.CatTimeJump, h.PID, ev.Index, "pts moved backwards")
		} else {
			a.lastTime = e.Value
		}
	}
	isKF := h.RandomAccess && h.PUSI
	if damaged {
		a.enterDamaged(ev.Index, faultCategoryOf(a.rep.Faults))
		return
	}
	if a.damaged {
		if !isKF {
			return // chain damage: wait for the next random-access point
		}
		a.sink.Isolate(a.dmgStart, ev.Index-1, a.dmgReason)
		a.rep.Isolations = append(a.rep.Isolations,
			Isolation{StartPacket: a.dmgStart, EndPacket: ev.Index - 1, Reason: a.dmgReason})
		a.damaged = false
		a.beginSegment(ev.Index)
	} else if !a.segOpen {
		a.beginSegment(ev.Index)
	} else if isKF && a.MaxSegmentPackets > 0 && a.segCount >= a.MaxSegmentPackets {
		a.endSegment(ev.Index - 1)
		a.beginSegment(ev.Index)
	}
	if isKF {
		a.addKeyframe(ev.Index)
	}
	a.segCount++
	ev.Media = true
	a.sink.Append(ev)
}

func (a *Analyzer) consumePSI(ev PacketEvent) {
	h := ev.Header
	asm := a.psi[h.PID]
	if asm == nil {
		asm = &ts.Assembler{}
		a.psi[h.PID] = asm
	}
	if h.PayloadOff < 0 {
		return
	}
	for _, sec := range asm.Push(ev.Raw[h.PayloadOff:], h.PUSI, ev.Index) {
		tr := a.tables[h.PID]
		if tr == nil {
			tr = &ts.TableTracker{}
			a.tables[h.PID] = tr
		}
		before := len(tr.Updates)
		first := !tr.HasTable()
		if !tr.Update(sec, h.PID, a.mediaTime()) {
			a.rep.Faults = append(a.rep.Faults, tr.Faults[len(tr.Faults)-1])
			continue
		}
		if len(tr.Updates) > before {
			a.rep.PSIUpdates++
		}
		if first || len(tr.Updates) > before {
			a.applyTable(h.PID, sec) // register once per table version
		}
	}
}

func (a *Analyzer) applyTable(pid uint16, sec ts.Section) {
	if pid == 0 && sec.TableID == 0x00 {
		for _, pmtPID := range ts.ParsePAT(sec) {
			a.pmtPIDs[pmtPID] = true
		}
		return
	}
	if sec.TableID == 0x02 {
		pcrPID, entries := ts.ParsePMT(sec)
		for _, e := range entries {
			a.mediaPIDs[e.PID] = true
		}
		a.programs = append(a.programs, Program{
			Number: sec.TableIDExt, PMTPID: pid, PCRPID: pcrPID, Streams: entries,
		})
	}
}

func (a *Analyzer) addKeyframe(idx uint64) {
	t := a.lastTime
	if n := len(a.rep.Keyframes); n > 0 {
		if d := t - a.rep.Keyframes[n-1].Time; d > a.rep.MaxKFInterval {
			a.rep.MaxKFInterval = d
		}
	}
	a.rep.Keyframes = append(a.rep.Keyframes, Keyframe{PacketIndex: idx, Time: t})
}

func (a *Analyzer) beginSegment(idx uint64) {
	a.segOpen, a.segStart, a.segCount = true, idx, 0
	a.sink.BeginSegment(a.segID, idx, a.lastTime)
}

func (a *Analyzer) endSegment(last uint64) {
	a.sink.EndSegment(a.segID, last, a.lastTime)
	a.rep.Segments++
	a.segID++
	a.segOpen = false
}

func (a *Analyzer) enterDamaged(idx uint64, cat fault.Category) {
	if a.segOpen {
		a.endSegment(idx - 1)
	}
	if !a.damaged {
		a.damaged, a.dmgStart, a.dmgReason = true, idx, cat
	}
}

// Finalize closes any open segment or damaged zone and returns the report.
func (a *Analyzer) Finalize(lastPacket uint64) Report {
	if a.segOpen {
		a.endSegment(lastPacket)
	}
	if a.damaged {
		a.sink.Isolate(a.dmgStart, lastPacket, a.dmgReason)
		a.rep.Isolations = append(a.rep.Isolations,
			Isolation{StartPacket: a.dmgStart, EndPacket: lastPacket, Reason: a.dmgReason})
		a.damaged = false
	}
	for pid, asm := range a.psi {
		if start, _, ok := asm.Pending(); ok {
			a.addFault(fault.CatPSIIncomplete, pid, start, "stream ended mid-section")
		}
	}
	a.rep.PCRWraps = a.pcrTL.Wraps()
	a.rep.Programs = a.programs
	return a.rep
}

func ccGapDetail(gap int) string {
	return "continuity counter jumped, missing=" + strconv.Itoa(gap)
}

func faultCategoryOf(fl fault.List) fault.Category {
	if len(fl) == 0 {
		return fault.CatUnrecoverable
	}
	return fl[len(fl)-1].Category
}
