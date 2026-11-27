package ts

import (
	"tsqc/internal/clock"
)

func (d *Demux) isPMTPID(pid uint16) bool {
	if d.tables.PAT() == nil {
		return false
	}
	for _, pmtPID := range d.tables.PAT().Programs {
		if pmtPID == pid {
			return true
		}
	}
	return false
}

func (d *Demux) assembleSection(p *Packet) ([]Section, error) {
	asm := d.sections[p.PID]
	if asm == nil {
		asm = &SectionAssembler{}
		d.sections[p.PID] = asm
	}
	return asm.Feed(p.Payload, p.PUSI)
}

func (d *Demux) reconfigure() error {
	pat := d.tables.PAT()
	if pat == nil {
		return nil
	}
	progs := d.tables.Programs()
	if len(progs) > 0 {
		d.pmtPID = pat.Programs[progs[0]]
	}
	pmt := d.tables.ActivePMT()
	if pmt == nil {
		return nil
	}
	d.streams = make(map[uint16]Kind)
	d.pcrPID = pmt.PCRPID
	for _, s := range pmt.Streams {
		switch s.Type {
		case StreamH264:
			d.streams[s.PID] = KindVideo
		case StreamAAC:
			d.streams[s.PID] = KindAudio
		}
	}
	if _, ok := d.streams[d.pcrPID]; !ok {
		for pid, k := range d.streams {
			if k == KindVideo {
				d.pcrPID = pid
			}
		}
	}
	return nil
}

func (d *Demux) timelineOf(pid uint16) *clock.Timeline {
	tl := d.timeline[pid]
	if tl == nil {
		tl = &clock.Timeline{}
		d.timeline[pid] = tl
	}
	return tl
}
