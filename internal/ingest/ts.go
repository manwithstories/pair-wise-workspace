package ingest

import (
	"errors"

	"mediad/internal/mediaerr"
)

const (
	PacketSize = 188
	SyncByte   = 0x47
	MaxPESSize = 1 << 20
)

// PCR is the raw 33-bit PCR base and 9-bit extension.
type PCR struct {
	Base      uint64
	Extension uint16
	Offset    int64
}

// Stamp is a raw 33-bit MPEG timestamp. Timeline performs unwrapping.
type Stamp struct {
	Value uint64
	Valid bool
}

// Record is one reassembled PES payload with its source byte offset.
type Record struct {
	Offset      int64
	PacketIndex int
	PID         uint16
	StreamID    uint8
	PTS         Stamp
	DTS         Stamp
	PCR         *PCR
	Payload     []byte
	KeyHint     bool
}

// BadPacket describes a sync or framing failure at an absolute offset.
type BadPacket struct {
	Offset int64
	PID    uint16
	Reason error
}

// CCGap is recorded exactly once for a discontinuous PID transition.
type CCGap struct {
	Offset   int64
	PID      uint16
	Expected uint8
	Got      uint8
	Missing  int
}

// Report contains lossless scanner diagnostics.
type Report struct {
	PacketCount      int
	ScannedPackets   int
	FirstBadOffset   int64
	BadPackets       []BadPacket
	Gaps             []CCGap
	DuplicatePackets int
	TruncatedOffset  int64
	TrailingBytes    int
	PSIRejected      int
}

type ccState struct {
	seen bool
	cc   uint8
}

type pesBuilder struct {
	rec      Record
	expected int // total PES bytes including start-code prefix; -1 when unknown
	header   int
	data     []byte
}

// Scanner performs bounded 188-byte sync scanning and PES reassembly.
type Scanner struct {
	MaxPES int
	Tables *PSIRegistry

	cc     map[uint16]ccState
	pes    map[uint16]*pesBuilder
	pcrs   map[uint16]*PCR
	report Report
}

// NewScanner creates a scanner with bounded reassembly buffers.
func NewScanner() *Scanner {
	return &Scanner{
		MaxPES: MaxPESSize,
		Tables: NewPSIRegistry(),
		cc:     make(map[uint16]ccState),
		pes:    make(map[uint16]*pesBuilder),
		pcrs:   make(map[uint16]*PCR),
		report: Report{FirstBadOffset: -1, TruncatedOffset: -1},
	}
}

// ScanBytes scans a complete or truncated TS byte slice. A structured
// mediaerr.KindTruncated error is returned for a partial tail, while all
// complete prefix packets and diagnostics remain available.
func ScanBytes(data []byte) ([]Record, *Report, error) {
	s := NewScanner()
	recs, err := s.Scan(data)
	rep := s.Report()
	return recs, rep, err
}

// Report returns a copy of the current scan report.
func (s *Scanner) Report() *Report {
	r := s.report
	r.BadPackets = append([]BadPacket(nil), s.report.BadPackets...)
	r.Gaps = append([]CCGap(nil), s.report.Gaps...)
	return &r
}

// Scan consumes all currently available bytes.
func (s *Scanner) Scan(data []byte) ([]Record, error) {
	var records []Record
	pos := 0
	for pos < len(data) {
		if len(data)-pos < PacketSize {
			s.report.TruncatedOffset = int64(pos)
			s.report.TrailingBytes = len(data) - pos
			return records, mediaerr.New(mediaerr.KindTruncated, int64(pos), 0, true,
				"partial TS packet: %d trailing bytes", len(data)-pos)
		}

		if data[pos] != SyncByte {
			off := int64(pos)
			if s.report.FirstBadOffset < 0 {
				s.report.FirstBadOffset = off
			}
			reason := mediaerr.New(mediaerr.KindInvalidPacket, off, 0, true,
				"expected sync 0x47, got 0x%02x", data[pos])
			s.report.BadPackets = append(s.report.BadPackets, BadPacket{Offset: off, Reason: reason})
			next := resync(data, pos+1)
			if next < 0 {
				s.report.TruncatedOffset = off
				s.report.TrailingBytes = len(data) - pos
				return records, mediaerr.Wrap(mediaerr.KindUnrecoverable, off, 0, false,
					errors.New("no valid TS sync candidate after bad packet"))
			}
			pos = next
			continue
		}

		pkt := data[pos : pos+PacketSize]
		packetIndex := s.report.PacketCount
		s.report.PacketCount++
		s.report.ScannedPackets++
		off := int64(pos)
		pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		pusi := pkt[1]&0x40 != 0
		afc := (pkt[3] >> 4) & 0x03
		cc := pkt[3] & 0x0f
		if pid == 0x1fff {
			// Null stuffing packets carry no PES/PSI and have no sequence state.
			pos += PacketSize
			continue
		}
		payloadStart := 4

		if afc == 2 || afc == 3 {
			pcr, n, err := parseAdaptation(pkt, off, pid)
			if err != nil {
				bad := mediaerr.Wrap(mediaerr.KindInvalidPacket, off, pid, true, err)
				s.report.BadPackets = append(s.report.BadPackets, BadPacket{Offset: off, Reason: bad})
			} else {
				payloadStart += n
				if pcr != nil {
					pcr.Offset = off
					s.pcrs[pid] = pcr
				}
			}
		}
		hasPayload := afc == 1 || afc == 3
		if afc == 0 || payloadStart > PacketSize {
			err := mediaerr.New(mediaerr.KindInvalidPacket, off, pid, true, "invalid adaptation control/payload offset")
			s.report.BadPackets = append(s.report.BadPackets, BadPacket{Offset: off, Reason: err})
			pos += PacketSize
			continue
		}

		if hasPayload {
			s.checkCC(pid, cc, off)
			payload := pkt[payloadStart:]
			if pid == 0 || s.Tables.IsPSI(pid) {
				if err := s.Tables.HandlePacket(pid, pusi, payload, off); err != nil {
					s.report.PSIRejected++
					s.report.BadPackets = append(s.report.BadPackets, BadPacket{Offset: off, PID: pid, Reason: err})
				}
			} else {
				if rec, ok, err := s.appendPES(pid, pusi, payload, off, packetIndex); err != nil {
					bad := mediaerr.Wrap(mediaerr.KindCorruptPayload, off, pid, true, err)
					s.report.BadPackets = append(s.report.BadPackets, BadPacket{Offset: off, Reason: bad})
					delete(s.pes, pid)
				} else if ok {
					records = append(records, rec)
				}
			}
		}
		pos += PacketSize
	}
	records = append(records, s.Flush()...)
	return records, nil
}

func (s *Scanner) checkCC(pid uint16, cc uint8, off int64) {
	st := s.cc[pid]
	if !st.seen {
		st.seen = true
		st.cc = cc
		s.cc[pid] = st
		return
	}
	expected := (st.cc + 1) & 0x0f
	switch {
	case cc == st.cc:
		s.report.DuplicatePackets++
	case cc != expected:
		missing := int((cc - expected) & 0x0f)
		if missing == 0 {
			missing = 15
		}
		s.report.Gaps = append(s.report.Gaps, CCGap{
			Offset: off, PID: pid, Expected: expected, Got: cc, Missing: missing,
		})
		st.cc = cc // adopt observed counter: later contiguous packets are not chain errors.
	default:
		st.cc = cc
	}
	s.cc[pid] = st
}

func resync(data []byte, start int) int {
	limit := min(start+PacketSize, len(data))
	for i := start; i < limit; i++ {
		if data[i] != SyncByte {
			continue
		}
		if i+PacketSize >= len(data) {
			return i
		}
		if data[i+PacketSize] == SyncByte {
			return i
		}
	}
	return -1
}

func parseAdaptation(pkt []byte, off int64, pid uint16) (*PCR, int, error) {
	if len(pkt) < 5 {
		return nil, 0, mediaerr.New(mediaerr.KindInvalidPacket, off, pid, true, "short adaptation field")
	}
	length := int(pkt[4])
	total := length + 1
	if length == 0 {
		return nil, total, nil
	}
	if total >= PacketSize-4 || 5+length > len(pkt) {
		return nil, total, mediaerr.New(mediaerr.KindInvalidPacket, off, pid, true, "adaptation length %d out of range", length)
	}
	flags := pkt[5]
	if flags&0x10 == 0 {
		return nil, total, nil
	}
	if length < 7 {
		return nil, total, mediaerr.New(mediaerr.KindInvalidPacket, off, pid, true, "PCR flag set but adaptation is short")
	}
	b := pkt[6:12]
	base := uint64(b[0])<<25 | uint64(b[1])<<17 | uint64(b[2])<<9 |
		uint64(b[3])<<1 | uint64(b[4])>>7
	ext := uint16(b[4]&1)<<8 | uint16(b[5])
	if base >= 1<<33 {
		return nil, total, mediaerr.New(mediaerr.KindTimestampOverflow, off, pid, true, "PCR base exceeds 33 bits")
	}
	return &PCR{Base: base, Extension: ext}, total, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
