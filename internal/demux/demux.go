// Package demux performs one streaming pass over a TS file and records
// elementary-stream statistics, continuity gaps and PSI history.
package demux

import (
	"context"
	"errors"
	"os"
	"sort"

	"github.com/asticode/go-astits"

	"tsprobe/internal/psi"
	"tsprobe/internal/tsio"
)

// Anomalies summarises all recoverable input problems seen during a scan.
type Anomalies struct {
	NonMultipleOf188   bool   `json:"non_multiple_of_188"`
	SyncLossEvents     uint64 `json:"sync_loss_events"`
	SyncLossBytes      uint64 `json:"sync_loss_bytes"`
	ZeroFillSegments   uint64 `json:"zero_fill_segments"`
	ZeroFillBytes      uint64 `json:"zero_fill_bytes"`
	TrailingBytes      uint64 `json:"trailing_bytes"`
	IncompletePackets  uint64 `json:"incomplete_packets"`
	CRCErrors          uint64 `json:"crc_errors"`
	IncompleteSections uint64 `json:"incomplete_sections"`
	IgnoredNextTables  uint64 `json:"ignored_current_next"`
	UnknownSections    uint64 `json:"unknown_sections"`
	TransportErrors    uint64 `json:"transport_errors"`
	PacketParseErrors  uint64 `json:"packet_parse_errors"`
	LegalDiscontinuity uint64 `json:"legal_discontinuities"`
}

// StreamStats aggregates one elementary PID across the complete file.
type StreamStats struct {
	PID              uint16 `json:"pid"`
	ProgramNumber    uint16 `json:"program_number"`
	StreamType       uint8  `json:"stream_type"`
	PacketCount      uint64 `json:"packet_count"`
	PayloadBytes     uint64 `json:"payload_bytes"`
	DuplicatePackets uint64 `json:"duplicate_packets"`
	LostPackets      uint64 `json:"lost_packets"`
}

// NullStats aggregates the null PID separately.
type NullStats struct {
	PID          uint16 `json:"pid"`
	PacketCount  uint64 `json:"packet_count"`
	PayloadBytes uint64 `json:"payload_bytes"`
}

// CCGap records one continuity-counter jump.
type CCGap struct {
	PID            uint16 `json:"pid"`
	StartPacket    uint64 `json:"start_packet"`
	EndPacket      uint64 `json:"end_packet"`
	PreviousOffset int64  `json:"previous_byte_offset"`
	CurrentOffset  int64  `json:"current_byte_offset"`
	ExpectedCC     uint8  `json:"expected_cc"`
	ActualCC       uint8  `json:"actual_cc"`
	MissingPackets uint64 `json:"missing_packets"`
}

// Keyframe identifies the first TS packet of an inspectable video keyframe PES.
type Keyframe struct {
	PacketIndex   uint64 `json:"packet_index"`
	ByteOffset    int64  `json:"byte_offset"`
	PID           uint16 `json:"pid"`
	ProgramNumber uint16 `json:"program_number"`
	StreamType    uint8  `json:"stream_type"`
	PTSMillis     int64  `json:"pts_ms"`
}

// Result contains the shared scan output consumed by report and slicer.
type Result struct {
	Path         string
	SizeBytes    int64
	PacketsTotal uint64
	DurationMS   int64
	Anomalies    Anomalies
	Store        *psi.Store
	Streams      []StreamStats
	Null         NullStats
	CCGaps       []CCGap
	LostPackets  uint64
	Keyframes    []Keyframe
}

type ccTracker struct {
	lastCC     uint8
	lastIndex  uint64
	lastOffset int64
	has        bool
}

type scanner struct {
	result  *Result
	store   *psi.Store
	reader  *tsio.Reader
	streams map[uint16]*StreamStats
	cc      map[uint16]*ccTracker
	minPTS  int64
	maxPTS  int64
	hasPTS  bool
}

// Scan opens path and performs a streaming scan.
func Scan(path string) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	r := tsio.New(f)
	s := &scanner{
		result: &Result{Path: path, SizeBytes: info.Size(), Null: NullStats{PID: 0x1fff}},
		store:  psi.NewStore(), reader: r, streams: make(map[uint16]*StreamStats), cc: make(map[uint16]*ccTracker),
	}
	dmx := astits.NewDemuxer(context.Background(), r, astits.DemuxerOptPacketSize(tsio.PacketSize))
	for {
		indexBefore := r.PacketIndex()
		p, err := dmx.NextPacket()
		if errors.Is(err, astits.ErrNoMorePackets) {
			break
		}
		if err != nil {
			s.result.Anomalies.PacketParseErrors++
			if r.PacketIndex() == indexBefore {
				break
			}
			continue
		}
		s.process(p, r.PacketIndex(), r.PacketOffset())
	}
	s.store.Finish()
	s.finish(info.Size())
	return s.result, nil
}

func (s *scanner) process(p *astits.Packet, idx uint64, off int64) {
	if p.Header.TransportErrorIndicator {
		s.result.Anomalies.TransportErrors++
	}
	pid := p.Header.PID
	pusi := p.Header.PayloadUnitStartIndicator
	payload := p.Payload
	if kind, ok := s.store.PSIKind(pid); ok {
		_ = kind
		s.store.HandlePacket(pid, payload, pusi, idx, off)
	}
	if pid == astits.PIDNull {
		s.result.Null.PacketCount++
		s.result.Null.PayloadBytes += uint64(len(payload))
		return
	}
	ref, isES := s.store.Elementary(pid)
	if isES {
		st := s.ensureStream(ref)
		st.PacketCount++
		st.PayloadBytes += uint64(len(payload))
	}
	duplicate := s.checkCC(pid, p, idx, off, isES)
	if isES && duplicate {
		s.streams[pid].DuplicatePackets++
	}
	if isES && pusi && isVideo(ref.StreamType) {
		if kf, ok := inspectKeyframe(payload, ref, idx, off); ok {
			s.result.Keyframes = append(s.result.Keyframes, kf)
			s.observePTS(kf.PTSMillis)
		}
	}
	if pusi {
		if pts, ok := parsePESData(payload); ok {
			s.observePTS(pts / 90)
		}
	}
}

func (s *scanner) ensureStream(ref psi.StreamRef) *StreamStats {
	st := s.streams[ref.PID]
	if st == nil {
		st = &StreamStats{PID: ref.PID}
		s.streams[ref.PID] = st
	}
	st.ProgramNumber = ref.ProgramNumber
	st.StreamType = ref.StreamType
	return st
}

func (s *scanner) checkCC(pid uint16, p *astits.Packet, idx uint64, off int64, isES bool) bool {
	if !p.Header.HasPayload {
		return false
	}
	tr := s.cc[pid]
	if tr == nil {
		tr = &ccTracker{}
		s.cc[pid] = tr
	}
	legal := p.AdaptationField != nil && p.AdaptationField.DiscontinuityIndicator
	if legal {
		s.result.Anomalies.LegalDiscontinuity++
		tr.lastCC, tr.lastIndex, tr.lastOffset, tr.has = p.Header.ContinuityCounter, idx, off, true
		return false
	}
	if !tr.has {
		tr.lastCC, tr.lastIndex, tr.lastOffset, tr.has = p.Header.ContinuityCounter, idx, off, true
		return false
	}
	if p.Header.ContinuityCounter == tr.lastCC {
		return true
	}
	expected := (tr.lastCC + 1) & 0x0f
	if p.Header.ContinuityCounter != expected {
		missing := uint64((p.Header.ContinuityCounter + 16 - expected) & 0x0f)
		if missing > 0 {
			gap := CCGap{
				PID: pid, StartPacket: tr.lastIndex, EndPacket: idx, PreviousOffset: tr.lastOffset,
				CurrentOffset: off, ExpectedCC: expected, ActualCC: p.Header.ContinuityCounter, MissingPackets: missing,
			}
			s.result.CCGaps = append(s.result.CCGaps, gap)
			s.result.LostPackets += missing
			if isES {
				s.streams[pid].LostPackets += missing
			}
		}
	}
	tr.lastCC, tr.lastIndex, tr.lastOffset = p.Header.ContinuityCounter, idx, off
	return false
}

func (s *scanner) observePTS(ms int64) {
	if ms <= 0 {
		return
	}
	if !s.hasPTS || ms < s.minPTS {
		s.minPTS = ms
	}
	if !s.hasPTS || ms > s.maxPTS {
		s.maxPTS = ms
	}
	s.hasPTS = true
}

func (s *scanner) finish(size int64) {
	c := s.result
	c.PacketsTotal = s.reader.PacketIndex()
	counters := s.reader.Counters()
	c.Anomalies.SyncLossEvents = counters.SyncLossEvents
	c.Anomalies.SyncLossBytes = counters.SyncLossBytes
	c.Anomalies.ZeroFillSegments = counters.ZeroFillSegments
	c.Anomalies.ZeroFillBytes = counters.ZeroFillBytes
	c.Anomalies.TrailingBytes = counters.TrailingBytes
	c.Anomalies.IncompletePackets = counters.IncompletePackets
	c.Anomalies.CRCErrors = s.store.CRCErrors()
	c.Anomalies.IncompleteSections = s.store.IncompleteSections()
	c.Anomalies.IgnoredNextTables = s.store.IgnoredNext()
	c.Anomalies.UnknownSections = s.store.UnknownSections()
	c.Anomalies.NonMultipleOf188 = size%tsio.PacketSize != 0
	c.Store = s.store
	c.Streams = make([]StreamStats, 0, len(s.streams))
	for _, st := range s.streams {
		c.Streams = append(c.Streams, *st)
	}
	sort.Slice(c.Streams, func(i, j int) bool { return c.Streams[i].PID < c.Streams[j].PID })
	sort.Slice(c.CCGaps, func(i, j int) bool {
		if c.CCGaps[i].PID != c.CCGaps[j].PID {
			return c.CCGaps[i].PID < c.CCGaps[j].PID
		}
		return c.CCGaps[i].StartPacket < c.CCGaps[j].StartPacket
	})
	sort.Slice(c.Keyframes, func(i, j int) bool { return c.Keyframes[i].PacketIndex < c.Keyframes[j].PacketIndex })
	if s.hasPTS && s.maxPTS >= s.minPTS {
		c.DurationMS = s.maxPTS - s.minPTS + 100
	}
}

func isVideo(streamType uint8) bool {
	switch streamType {
	case 0x01, 0x02, 0x10, 0x1b, 0x24, 0x27, 0x42:
		return true
	default:
		return false
	}
}
