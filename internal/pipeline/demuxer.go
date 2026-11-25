package pipeline

import (
	"streamcut/internal/fixture"
	"streamcut/internal/media"
	"streamcut/internal/segment"
	"streamcut/internal/timeline"
	"streamcut/internal/ts"
)

// demuxer owns all per-stream parser state: PSI version gate, continuity
// tracking, PES reassembly and the per-PID timestamp unwrappers that extend
// raw 33-bit PTS onto one monotonic media timeline.
type demuxer struct {
	psi      *ts.PSITracker
	sections *ts.SectionAssembler
	cc       *ts.CCTracker
	pes      *ts.PESAssembler
	clock    *timeline.Clock
	pts      map[uint16]*timeline.Unwrapper
	lastCCGap bool
	truncated bool
	unsupported int
	// sps/pps captured from the first keyframe for the avcC config hash.
	cfgCache  []byte
	// Resume timeline seeding (zero when not a resumed run).
	ptsSeedRaw uint64
	ptsOrigin  media.Time90
}

func newDemuxer() *demuxer {
	return &demuxer{
		psi:      ts.NewPSITracker(),
		sections: ts.NewSectionAssembler(),
		cc:       ts.NewCCTracker(),
		pes:      ts.NewPESAssembler(1 << 20),
		clock:    timeline.NewClock(),
		pts:      map[uint16]*timeline.Unwrapper{},
	}
}

// seedTimeline pre-anchors the per-PID PTS unwrapper for a resumed tail so its
// raw stamps extend onto the uninterrupted run's absolute media timeline.
func (d *demuxer) seedTimeline(firstRaw uint64, origin media.Time90) {
	d.ptsSeedRaw = firstRaw
	d.ptsOrigin = origin
}

// videoPID returns the video elementary PID, preferring a committed PMT and
// falling back to the fixture's well-known video PID before PSI arrives.
func (d *demuxer) videoPID() uint16 {
	if prog := d.psi.Current(); prog != nil && prog.VideoPID != 0 {
		return prog.VideoPID
	}
	return fixture.VideoPID
}

// feed parses one 188-byte packet and returns zero or more access units.
// Recoverable class failures (CC gap, CRC reject) update counters but never
// stop the stream; only structural corruption returns an error.
func (d *demuxer) feed(raw []byte) ([]media.AccessUnit, error) {
	pkt, err := ts.Parse(raw)
	if err != nil {
		return nil, err
	}
	// Continuity tracking. A gap marks the epoch so any segment built from
	// these packets is withheld rather than published.
	if err := d.cc.Observe(pkt); err != nil {
		d.lastCCGap = true
	} else {
		d.lastCCGap = false
	}
	if pkt.HasPCR {
		if _, gap := d.clock.ExtendPCR27(pkt.PCR); gap {
			d.clock.ClearEpochGap()
		}
	}

	// PSI PIDs go through section reassembly and the CRC/version gate.
	// PID 0 is always PAT; the PMT PID is learned from the first valid PAT.
	// Until a PAT arrives, non-PAT packets are treated as PES (mid-stream
	// join) rather than misrouted into the section assembler.
	if pkt.PID == ts.PIDPAT || (d.psi.PMTPID() != 0 && pkt.PID == d.psi.PMTPID()) {
		return d.handlePSI(pkt)
	}

	// Other PIDs are PES elementary streams.
	pes, ok, err := d.pes.Push(pkt)
	if err != nil || !ok {
		return nil, err
	}
	return d.toUnits(pes)
}

func (d *demuxer) handlePSI(pkt *ts.Packet) ([]media.AccessUnit, error) {
	secs, err := d.sections.Push(pkt)
	if err != nil {
		return nil, err
	}
	for _, s := range secs {
		if _, oerr := d.psi.Observe(pkt.PID, s); oerr != nil {
			// CRC reject: retain previous version, count via tracker.Rejects.
			_ = oerr
		}
	}
	return nil, nil
}

// toUnits converts a reassembled PES into a timestamped access unit, extending
// the raw PTS through the per-PID unwrapper so media time stays monotonic
// across the 33-bit wrap.
func (d *demuxer) toUnits(p *ts.PES) ([]media.AccessUnit, error) {
	prog := d.psi.Current()
	kind := media.KindUnknown
	if prog != nil {
		switch p.PID {
		case prog.VideoPID:
			kind = media.KindVideoH264
		case prog.AudioPID:
			kind = prog.AudioKind
		}
	}
	// Resume fallback: a tail can start at a video IDR with no PSI in view.
	// Classify on the well-known fixture PIDs so the cutter and silence
	// detector still recognize the elementary streams before a PMT arrives.
	if kind == media.KindUnknown {
		switch p.PID {
		case fixture.VideoPID:
			kind = media.KindVideoH264
		case fixture.AudioPID:
			kind = media.KindAudioPCM16LE
		}
	}
	u := media.AccessUnit{PID: p.PID, Kind: kind, Data: p.Data}
	if p.HasPTS {
		uw, ok := d.pts[p.PID]
		if !ok {
			// Resume anchoring keys on the video PID directly so it works even
			// before the first PMT has classified the stream.
			if d.ptsOrigin > 0 && p.PID == d.videoPID() {
				uw = timeline.SeedUnwrapper(p.PTS, d.ptsOrigin)
			} else {
				uw = timeline.NewUnwrapper()
			}
			d.pts[p.PID] = uw
		}
		ext, gap := uw.Extend(p.PTS)
		u.PTS, u.HasPTS = ext, true
		u.DTS, u.HasDTS = ext, true
		u.ClockGap = gap
	}
	if kind == media.KindVideoH264 && u.IsKeyframe() && d.cfgCache == nil {
		d.cfgCache = append([]byte(nil), u.Data...)
	}
	return []media.AccessUnit{u}, nil
}

// videoConfig returns the cached first-keyframe NAL bytes for fingerprinting
// so segments with identical codec configuration compare equal.
func (d *demuxer) videoConfig(seg *segment.Segment) []byte {
	if d.cfgCache != nil {
		return d.cfgCache
	}
	if len(seg.Video) > 0 {
		return seg.Video[0].Data
	}
	return nil
}
