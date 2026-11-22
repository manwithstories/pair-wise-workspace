// Package rtp parses the framed RTP/RTCP packet stream produced by remote
// recording sites. The stream is a sequence of records:
//
//	[type:1][track:4 BE][length:2 BE][payload:length]
//
// type 1 carries an RTP packet, type 2 an RTCP sender report. Parsing
// validates every record, recovers from corrupt framing by resyncing
// byte-by-byte, and reports every anomaly through the shared fault
// model with exact byte offsets.
package rtp

import (
	"bufio"
	"encoding/binary"
	"io"

	"medpipe/internal/fault"

	"github.com/pion/rtp"
)

// Record types in the framed stream.
const (
	RecRTP  byte = 1
	RecRTCP byte = 2
)

const (
	// HeaderLen is the framed-record header size in bytes.
	HeaderLen = 7
	// MaxPayload bounds a single record payload; anything larger is
	// treated as corrupt framing.
	MaxPayload = 16384
	// MaxTracks bounds how many distinct tracks a stream may carry.
	MaxTracks = 64
)

// Record is one parsed stream record.
type Record struct {
	Type   byte
	Track  uint32
	Offset int64 // byte offset of the record header in the stream
	Packet *rtp.Packet
	SR     *SenderReport
}

// Stats summarizes a parsed stream.
type Stats struct {
	Records        int
	RTPPackets     int
	RTCPReports    int
	CorruptRecords int
	// BadOffsetBytes counts input bytes that could not be attributed to
	// any valid record (resync skips plus trailing garbage).
	BadOffsetBytes int64
	// TracksSeen is the set of track ids encountered (bounded by
	// MaxTracks; excess tracks are rejected).
	TracksSeen map[uint32]bool
}

// Parser reads records from a stream.
type Parser struct {
	br     *bufio.Reader
	off    int64 // offset of the next unconsumed byte
	faults *fault.List
	Stats  Stats
}

// NewParser wraps r. Faults are appended to fl (may be nil).
func NewParser(r io.Reader, fl *fault.List) *Parser {
	if fl == nil {
		fl = &fault.List{}
	}
	return &Parser{br: bufio.NewReader(r), faults: fl,
		Stats: Stats{TracksSeen: map[uint32]bool{}}}
}

// Faults returns the fault list the parser appends to.
func (p *Parser) Faults() *fault.List { return p.faults }

// Next returns the next valid record, skipping and accounting for
// corrupt bytes. It returns io.EOF when the stream is exhausted.
func (p *Parser) Next() (*Record, error) {
	for {
		hdr, err := p.br.Peek(HeaderLen)
		if err != nil {
			// Fewer than HeaderLen bytes remain: a truncated tail.
			if n := len(hdr); n > 0 {
				p.Stats.BadOffsetBytes += int64(n)
				p.off += int64(n)
				_, _ = p.br.Discard(n)
				p.faults.Add(fault.New(fault.KindCorrupt, fault.Unknown,
					fault.Unknown, p.off-int64(n), true,
					"truncated record at end of stream"))
			}
			return nil, io.EOF
		}
		start := p.off
		typ := hdr[0]
		track := binary.BigEndian.Uint32(hdr[1:5])
		length := int(binary.BigEndian.Uint16(hdr[5:7]))
		if (typ != RecRTP && typ != RecRTCP) || length > MaxPayload {
			// Implausible header: skip one byte and try to resync.
			p.Stats.BadOffsetBytes++
			p.Stats.CorruptRecords++
			p.off++
			_, _ = p.br.Discard(1)
			p.faults.Add(fault.New(fault.KindCorrupt, fault.Unknown,
				fault.Unknown, start, true, "invalid record header, resyncing"))
			continue
		}
		_, _ = p.br.Discard(HeaderLen)
		p.off += HeaderLen
		payload := make([]byte, length)
		n, err := io.ReadFull(p.br, payload)
		p.off += int64(n)
		if err != nil {
			p.Stats.BadOffsetBytes += int64(n)
			p.faults.Add(fault.New(fault.KindCorrupt, int64(track),
				fault.Unknown, start, true,
				"truncated record payload at end of stream"))
			return nil, io.EOF
		}
		if !p.trackAllowed(track, start) {
			continue
		}
		rec, ok := p.decode(typ, track, start, payload)
		if !ok {
			continue
		}
		return rec, nil
	}
}

func (p *Parser) trackAllowed(track uint32, off int64) bool {
	if p.Stats.TracksSeen[track] {
		return true
	}
	if len(p.Stats.TracksSeen) >= MaxTracks {
		p.faults.Add(fault.New(fault.KindResource, int64(track),
			fault.Unknown, off, true, "track limit exceeded, record dropped"))
		return false
	}
	p.Stats.TracksSeen[track] = true
	return true
}

func (p *Parser) decode(typ byte, track uint32, off int64, payload []byte) (*Record, bool) {
	p.Stats.Records++
	switch typ {
	case RecRTP:
		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(payload); err != nil {
			p.Stats.CorruptRecords++
			p.faults.Add(fault.Wrap(fault.KindCorrupt, int64(track),
				fault.Unknown, off, true, err, "invalid RTP packet"))
			return nil, false
		}
		p.Stats.RTPPackets++
		return &Record{Type: typ, Track: track, Offset: off, Packet: pkt}, true
	case RecRTCP:
		sr, err := ParseSenderReport(payload, track, off)
		if err != nil {
			p.Stats.CorruptRecords++
			p.faults.Add(err)
			return nil, false
		}
		p.Stats.RTCPReports++
		return &Record{Type: typ, Track: track, Offset: off, SR: sr}, true
	}
	return nil, false
}
