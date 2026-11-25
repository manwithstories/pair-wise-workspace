package ts

import (
	"fmt"

	"mediqc/internal/media"
)

// ElementarySample is a demultiplexed compressed access unit.
type ElementarySample = media.Sample

// DemuxResult contains parsed PSI and elementary samples.
type DemuxResult struct {
	Report       SyncReport
	PAT          *PAT
	PMT          *PMT
	Samples      []media.Sample
	CCGaps       int
	PCRWraps     int
	FirstDesync  int64
	DriftPPM     float64
	UnwrappedPCR []int64
}

type pesBuilder struct {
	buf       []byte
	pts       int64
	dts       int64
	havePTS   bool
	haveDTS   bool
	random    bool
	pesLength int
	want      int
	headerLen int
}

// Demuxer parses fixed-framing MPEG-TS into PSI and PES samples.
type Demuxer struct {
	Limits    media.Limits
	Collector *media.Collector
	CaseID    string
	TraceID   string
	Registry  *Registry

	pmtPID       uint16
	assemblers   map[uint16]*SectionAssembler
	lastCC       map[uint16]byte
	haveCC       map[uint16]bool
	pes          map[uint16]*pesBuilder
	ptsWrap      map[uint16]*media.WrapExtender
	dtsWrap      map[uint16]*media.WrapExtender
	pcrWrap      *media.WrapExtender
	drift        *media.DriftEstimator
	packetSize   int
	samples      []media.Sample
	CCGaps       int
	PCRWraps     int
	FirstDesync  int64
	UnwrappedPCR []int64
}

// NewDemuxer constructs a stateful TS demultiplexer.
func NewDemuxer(limits media.Limits, collector *media.Collector) *Demuxer {
	return &Demuxer{
		Limits:      limits,
		Collector:   collector,
		Registry:    &Registry{},
		assemblers:  map[uint16]*SectionAssembler{},
		lastCC:      map[uint16]byte{},
		haveCC:      map[uint16]bool{},
		pes:         map[uint16]*pesBuilder{},
		ptsWrap:     map[uint16]*media.WrapExtender{},
		dtsWrap:     map[uint16]*media.WrapExtender{},
		pcrWrap:     media.NewPTSWrapExtender(),
		drift:       media.NewDriftEstimator(64),
		FirstDesync: -1,
	}
}

// Demux is a one-shot helper around Feed.
func Demux(data []byte, limits media.Limits, collector *media.Collector) (*DemuxResult, *media.Error) {
	d := NewDemuxer(limits, collector)
	if err := d.Feed(data); err != nil {
		return d.Result(), err
	}
	if err := d.FlushPES(); err != nil {
		return d.Result(), err
	}
	return d.Result(), nil
}

// Feed parses all complete packets in data.
func (d *Demuxer) Feed(data []byte) *media.Error {
	report, serr := DetectSync(data, d.Limits)
	d.setFirstDesync(report.FirstDesync)
	if serr != nil && serr.Fatal {
		d.add(serr)
		return serr
	}
	if serr != nil {
		d.add(serr)
	}
	d.packetSize = report.PacketSize
	if report.PacketSize == 0 {
		d.packetSize = Size188
	}
	complete := len(data) / d.packetSize
	for i := 0; i < complete; i++ {
		off := i * d.packetSize
		if perr := d.parsePacket(data[off:off+Size188], int64(off)); perr != nil {
			if perr.Fatal {
				return perr
			}
			d.add(perr)
		}
	}
	if rem := len(data) % d.packetSize; rem != 0 {
		err := &media.Error{
			Code: media.CodeUnexpectedEOF, Component: "ts.demux", Offset: int64(complete * d.packetSize),
			Message: fmt.Sprintf("truncated %d byte packet remainder", rem),
		}
		d.add(err.MarkFatal())
		return err
	}
	return nil
}

// Result exports accumulated parser state.
func (d *Demuxer) Result() *DemuxResult {
	res := &DemuxResult{
		PAT: d.Registry.PAT, PMT: d.Registry.TrustedPMT,
		Samples: append([]media.Sample(nil), d.samples...),
		CCGaps:  d.CCGaps, PCRWraps: d.PCRWraps, FirstDesync: d.FirstDesync,
		DriftPPM: d.drift.DriftPPM(), UnwrappedPCR: append([]int64(nil), d.UnwrappedPCR...),
	}
	return res
}

func (d *Demuxer) parsePacket(pkt []byte, offset int64) *media.Error {
	if len(pkt) < Size188 || pkt[0] != SyncByte {
		return (&media.Error{Code: media.CodeSyncLost, Component: "ts.demux", Offset: offset, Message: "packet sync byte missing"}).MarkFatal()
	}
	pusi := pkt[1]&0x40 != 0
	pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
	afc := (pkt[3] >> 4) & 0x03
	cc := pkt[3] & 0x0f
	d.checkCC(pid, cc, offset)

	payloadStart := 4
	if afc == 2 || afc == 3 {
		if len(pkt) < 5 {
			return d.lengthError(offset, "adaptation field length missing")
		}
		alen := int(pkt[4])
		if 5+alen > len(pkt) {
			return d.lengthError(offset, "adaptation field overruns packet")
		}
		if alen > 0 && pkt[5]&0x10 != 0 && alen >= 7 {
			d.parsePCR(pkt[6:12], offset)
		}
		payloadStart = 5 + alen
	}
	if afc == 2 || payloadStart >= len(pkt) {
		return nil
	}
	payload := pkt[payloadStart:]

	if pid == PATPID || (d.pmtPID != 0 && pid == d.pmtPID) {
		return d.feedPSI(pid, pusi, payload, offset)
	}
	if d.Registry.TrustedPMT != nil && d.isElementaryPID(pid) {
		return d.feedPES(pid, pusi, payload, offset)
	}
	return nil
}

func (d *Demuxer) checkCC(pid uint16, cc byte, offset int64) {
	if last, ok := d.lastCC[pid]; ok && d.haveCC[pid] {
		expected := (last + 1) & 0x0f
		if cc != expected && cc != last {
			d.CCGaps++
			d.add((&media.Error{
				Code: media.CodeCCGap, Component: "ts.demux", Offset: offset,
				Message: fmt.Sprintf("PID %d CC %d expected %d", pid, cc, expected),
			}).MarkRecoverable())
		}
	}
	d.lastCC[pid] = cc
	d.haveCC[pid] = true
}

func (d *Demuxer) parsePCR(b []byte, offset int64) {
	if len(b) < 6 {
		return
	}
	base := uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 | uint64(b[3])<<1 | uint64(b[4])>>7
	full := d.pcrWrap.Extend(base)
	if d.pcrWrap.Wraps > d.PCRWraps {
		d.PCRWraps = d.pcrWrap.Wraps
		d.add((&media.Error{
			Code: media.CodeClockWrap, Component: "ts.demux", Offset: offset,
			Message: "PCR 33-bit epoch wrap expanded",
		}).MarkRecoverable())
	}
	d.UnwrappedPCR = append(d.UnwrappedPCR, full)
	d.drift.Observe(full, full)
}

func (d *Demuxer) feedPSI(pid uint16, pusi bool, payload []byte, offset int64) *media.Error {
	a := d.assemblers[pid]
	if a == nil {
		a = &SectionAssembler{}
		d.assemblers[pid] = a
	}
	sections, err := a.Push(pusi, payload, d.Limits)
	if err != nil {
		err.Offset = offset
		d.add(err)
		return nil // corrupt PSI must not abort the whole batch case.
	}
	for _, raw := range sections {
		if pid == PATPID {
			pat, perr := d.Registry.ApplyPAT(raw, d.Limits)
			if perr != nil {
				perr.Offset = offset
				d.add(perr)
				continue
			}
			d.pmtPID = pat.PMTPID
		} else {
			_, perr := d.Registry.ApplyPMT(raw, d.Limits)
			if perr != nil {
				perr.Offset = offset
				d.add(perr.MarkRecoverable())
			}
		}
	}
	return nil
}

func (d *Demuxer) isElementaryPID(pid uint16) bool {
	for _, s := range d.Registry.TrustedPMT.Streams {
		if s.PID == pid {
			return true
		}
	}
	return false
}

func (d *Demuxer) lengthError(offset int64, msg string) *media.Error {
	err := &media.Error{Code: media.CodeLengthInvalid, Component: "ts.demux", Offset: offset, Message: msg}
	return err.MarkFatal()
}

func (d *Demuxer) add(err *media.Error) {
	if err == nil {
		return
	}
	if d.CaseID != "" {
		err.CaseID = d.CaseID
	}
	if d.TraceID != "" && err.TraceID == "" {
		err.TraceID = d.TraceID
	}
	if d.Collector != nil {
		d.Collector.Add(err)
	}
}

func (d *Demuxer) setFirstDesync(v int64) {
	if d.FirstDesync == -1 || (v >= 0 && v < d.FirstDesync) {
		d.FirstDesync = v
	}
}

// Temporary PES implementation is added in the next small batch.
func (d *Demuxer) feedPES(pid uint16, pusi bool, payload []byte, offset int64) *media.Error {
	return nil
}

// Temporary flush stub.
func (d *Demuxer) FlushPES() *media.Error { return nil }
