package capture

import (
	"sort"

	"mediarig/internal/media"
)

// Gap is a run of one or more missing RTP sequence numbers.
type Gap struct {
	SSRC        uint32
	PrevSeq     uint64
	NextSeq     uint64
	MissingFrom uint64
	MissingTo   uint64
	Count       int
}

// TrackStats contains stable packet/recovery counters.
type TrackStats struct {
	SSRC             uint32  `json:"ssrc"`
	PayloadType      uint8   `json:"payload_type"`
	Format           string  `json:"format"`
	ClockRate        int     `json:"clock_rate"`
	PacketsReceived  int     `json:"packets_received"`
	UniquePackets    int     `json:"unique_packets"`
	DuplicatePackets int     `json:"duplicate_packets"`
	ReorderedPackets int     `json:"reordered_packets"`
	MissingPackets   int     `json:"missing_packets"`
	ExpectedPackets  int     `json:"expected_packets"`
	LossRate         float64 `json:"loss_rate"`
}

// Track is one SSRC-selected RTP stream.
type Track struct {
	Spec    PayloadSpec
	SSRC    uint32
	Packets []*Packet
	Gaps    []Gap
	Stats   TrackStats

	reports        []SenderReport
	rawTSExt       map[uint32]uint64
	bySequence     map[uint64]*Packet
	seqInitialized bool
	maxSequence    uint64
	tsInitialized  bool
	maxTimestamp   uint64
}

// SenderReport returns the first SR for the track, if present.
func (t *Track) SenderReport() (SenderReport, bool) {
	if len(t.reports) == 0 {
		return SenderReport{}, false
	}
	return t.reports[0], true
}

// Assembler parses records into SSRC tracks and restores finite reordering.
type Assembler struct {
	Registry map[uint8]PayloadSpec
	Tracks   map[uint32]*Track
	Window   int
	reports  map[uint32][]SenderReport
}

func NewAssembler(registry map[uint8]PayloadSpec) *Assembler {
	if registry == nil {
		registry = DefaultPayloads()
	}
	return &Assembler{Registry: registry, Tracks: map[uint32]*Track{}, Window: 64, reports: map[uint32][]SenderReport{}}
}

// IngestRecords parses all RTP/RTCP records. Order counters are updated while
// reading so adjacent and finite-window swaps are counted accurately.
func (a *Assembler) IngestRecords(records []Record) error {
	for _, rec := range records {
		switch rec.Type {
		case RecordRTCP:
			srs, err := ParseSenderReports(rec)
			if err != nil {
				return err
			}
			for _, sr := range srs {
				a.reports[sr.SSRC] = append(a.reports[sr.SSRC], sr)
				if tr := a.Tracks[sr.SSRC]; tr != nil {
					tr.reports = append(tr.reports, sr)
				}
			}
		case RecordRTP:
			pkt, err := ParseRTP(rec)
			if err != nil {
				return err
			}
			spec, ok := a.Registry[pkt.Packet.PayloadType]
			if !ok {
				seq := pkt.Sequence()
				err := media.Corrupt(media.StageRTP, "unsupported RTP payload type %d", pkt.Packet.PayloadType)
				err = media.WithRecordIndex(err, rec.Index)
				err = media.WithPacketSequence(err, seq)
				return err
			}
			tr := a.trackFor(pkt.SSRC(), spec)
			a.ingestPacket(tr, pkt)
		}
	}
	return nil
}

func (a *Assembler) trackFor(ssrc uint32, spec PayloadSpec) *Track {
	tr, ok := a.Tracks[ssrc]
	if !ok {
		tr = &Track{
			Spec:       spec,
			SSRC:       ssrc,
			rawTSExt:   map[uint32]uint64{},
			bySequence: map[uint64]*Packet{},
		}
		a.Tracks[ssrc] = tr
		if rs := a.reports[ssrc]; len(rs) > 0 {
			tr.reports = append(tr.reports, rs...)
		}
	}
	return tr
}

func (a *Assembler) ingestPacket(tr *Track, pkt *Packet) {
	seq := pkt.Sequence()
	if !tr.seqInitialized {
		pkt.ExtSequence = uint64(seq)
		tr.seqInitialized = true
		tr.maxSequence = pkt.ExtSequence
	} else {
		pkt.ExtSequence = ExtendUint16(tr.maxSequence, seq, true)
		if pkt.ExtSequence < tr.maxSequence {
			tr.Stats.ReorderedPackets++
		}
		if pkt.ExtSequence > tr.maxSequence {
			tr.maxSequence = pkt.ExtSequence
		}
	}

	ts := pkt.Timestamp()
	if !tr.tsInitialized {
		pkt.ExtTimestamp = uint64(ts)
		tr.tsInitialized = true
		tr.maxTimestamp = pkt.ExtTimestamp
	} else {
		pkt.ExtTimestamp = ExtendUint32(tr.maxTimestamp, ts, true)
		if pkt.ExtTimestamp > tr.maxTimestamp {
			tr.maxTimestamp = pkt.ExtTimestamp
		}
	}
	tr.rawTSExt[ts] = pkt.ExtTimestamp

	tr.Stats.PacketsReceived++
	if _, exists := tr.bySequence[pkt.ExtSequence]; exists {
		tr.Stats.DuplicatePackets++
		return
	}
	tr.bySequence[pkt.ExtSequence] = pkt
	tr.Stats.UniquePackets++
}

// Finish sorts each track and computes sequence gaps and loss statistics.
func (a *Assembler) Finish() error {
	for _, tr := range a.Tracks {
		seqs := make([]uint64, 0, len(tr.bySequence))
		for seq := range tr.bySequence {
			seqs = append(seqs, seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		tr.Packets = tr.Packets[:0]
		for _, seq := range seqs {
			tr.Packets = append(tr.Packets, tr.bySequence[seq])
		}
		tr.Gaps = tr.Gaps[:0]
		missing := 0
		for i := 1; i < len(tr.Packets); i++ {
			prev := tr.Packets[i-1]
			next := tr.Packets[i]
			gapCount := int(next.ExtSequence - prev.ExtSequence - 1)
			if gapCount <= 0 {
				continue
			}
			g := Gap{
				SSRC:        tr.SSRC,
				PrevSeq:     prev.ExtSequence,
				NextSeq:     next.ExtSequence,
				MissingFrom: prev.ExtSequence + 1,
				MissingTo:   next.ExtSequence - 1,
				Count:       gapCount,
			}
			tr.Gaps = append(tr.Gaps, g)
			missing += gapCount
		}
		tr.Stats.SSRC = tr.SSRC
		tr.Stats.PayloadType = tr.Spec.PayloadType
		tr.Stats.Format = string(tr.Spec.Format)
		tr.Stats.ClockRate = tr.Spec.ClockRate
		tr.Stats.MissingPackets = missing
		if len(tr.Packets) > 0 {
			expected := int(tr.Packets[len(tr.Packets)-1].ExtSequence-tr.Packets[0].ExtSequence) + 1
			tr.Stats.ExpectedPackets = expected
			tr.Stats.LossRate = float64(missing) / float64(expected)
		}
	}
	return nil
}

// GapSamples derives the RTP timestamp increment per packet from the packets
// bracketing a gap. It returns zero when timestamps do not describe audio.
func (t *Track) GapSamples(g Gap) int {
	var prev, next *Packet
	for _, p := range t.Packets {
		if p.ExtSequence == g.PrevSeq {
			prev = p
		}
		if p.ExtSequence == g.NextSeq {
			next = p
		}
	}
	if prev == nil || next == nil || next.ExtTimestamp <= prev.ExtTimestamp {
		return 0
	}
	seqSteps := g.NextSeq - g.PrevSeq
	tsDelta := next.ExtTimestamp - prev.ExtTimestamp
	if seqSteps == 0 || tsDelta%seqSteps != 0 {
		return 0
	}
	return int(tsDelta / seqSteps * uint64(g.Count))
}

// JitterBuffer restores a bounded out-of-order run. It is intentionally
// deterministic and offline-friendly; Flush releases the final partial window.
type JitterBuffer struct {
	Window int
	next   *uint64
	items  map[uint64]*Packet
}

func NewJitterBuffer(window int) *JitterBuffer {
	if window <= 0 {
		window = 64
	}
	return &JitterBuffer{Window: window, items: map[uint64]*Packet{}}
}

// Push inserts a packet and returns all now-contiguous packets.
func (j *JitterBuffer) Push(p *Packet) []*Packet {
	if j.next == nil {
		n := p.ExtSequence
		j.next = &n
	}
	if _, exists := j.items[p.ExtSequence]; exists {
		return nil
	}
	j.items[p.ExtSequence] = p
	return j.drain()
}

func (j *JitterBuffer) drain() []*Packet {
	var out []*Packet
	for j.next != nil {
		p, ok := j.items[*j.next]
		if !ok {
			break
		}
		out = append(out, p)
		delete(j.items, *j.next)
		n := *j.next + 1
		j.next = &n
	}
	return out
}

// Flush returns any remaining packets sorted by extended sequence.
func (j *JitterBuffer) Flush() []*Packet {
	out := j.drain()
	seqs := make([]uint64, 0, len(j.items))
	for seq := range j.items {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, k int) bool { return seqs[i] < seqs[k] })
	for _, seq := range seqs {
		out = append(out, j.items[seq])
	}
	j.items = map[uint64]*Packet{}
	return out
}
