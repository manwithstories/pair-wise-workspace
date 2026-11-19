// Package packet reads and validates the framed RTP/RTCP capture format.
//
// Stream layout: an optional session record followed by packet records.
// Each record is [kind:1][length:2 BE][payload].
//
//	kind 0: session description (JSON: track clock rates, kinds, deps)
//	kind 1: RTP packet (pion/rtp wire format)
//	kind 2: RTCP Sender Report (pion/rtcp wire format)
//	kind 3: truncated RTP record (fewer than 12 header bytes survived)
package packet

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

const (
	KindSession = 0
	KindRTP     = 1
	KindRTCP    = 2
	KindTrunc   = 3
)

// Hard input limits; exceeding any of them rejects the input (exit 2).
const (
	MaxRecords    = 2_000_000
	MaxTracks     = 16
	MaxPacketSize = 65535
)

// Session describes the tracks carried by the stream.
type Session struct {
	Tracks []TrackDesc `json:"tracks"`
}

// TrackDesc maps an SSRC to its clock rate and media kind.
type TrackDesc struct {
	SSRC      uint32   `json:"ssrc"`
	ClockRate uint32   `json:"rate"`
	Kind      string   `json:"kind"` // "video" or "audio"
	DependsOn []uint32 `json:"deps,omitempty"`
}

// Record is one parsed input record.
type Record struct {
	Index    int // 0-based position in the stream
	Kind     uint8
	RTP      *rtp.Packet
	SR       *rtcp.SenderReport
	Session  *Session
	Raw      []byte
	Truncated bool // header damaged; payload could not be parsed
}

// ErrLimit is returned when an input limit is exceeded.
var ErrLimit = errors.New("input limit exceeded")

// Reader decodes records from an io.Reader.
type Reader struct {
	r io.Reader
	n int
}

func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// Next returns the next record, or io.EOF at end of stream.
func (rd *Reader) Next() (*Record, error) {
	if rd.n >= MaxRecords {
		return nil, fmt.Errorf("%w: more than %d records", ErrLimit, MaxRecords)
	}
	var hdr [3]byte
	if _, err := io.ReadFull(rd.r, hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("record %d: truncated record header", rd.n)
		}
		return nil, err // includes io.EOF
	}
	kind, ln := hdr[0], int(binary.BigEndian.Uint16(hdr[1:3]))
	if ln > MaxPacketSize {
		return nil, fmt.Errorf("%w: record %d size %d", ErrLimit, rd.n, ln)
	}
	payload := make([]byte, ln)
	if _, err := io.ReadFull(rd.r, payload); err != nil {
		return nil, fmt.Errorf("record %d: truncated payload: %w", rd.n, err)
	}
	rec := &Record{Index: rd.n, Kind: kind, Raw: payload}
	rd.n++
	switch kind {
	case KindSession:
		var s Session
		if err := json.Unmarshal(payload, &s); err != nil {
			return nil, fmt.Errorf("record %d: bad session JSON: %w", rec.Index, err)
		}
		if len(s.Tracks) > MaxTracks {
			return nil, fmt.Errorf("%w: %d tracks", ErrLimit, len(s.Tracks))
		}
		rec.Session = &s
	case KindRTP:
		var p rtp.Packet
		if err := p.Unmarshal(payload); err != nil {
			return nil, fmt.Errorf("record %d: bad RTP: %w", rec.Index, err)
		}
		rec.RTP = &p
	case KindRTCP:
		pkts, err := rtcp.Unmarshal(payload)
		if err != nil {
			return nil, fmt.Errorf("record %d: bad RTCP: %w", rec.Index, err)
		}
		sr, ok := pkts[0].(*rtcp.SenderReport)
		if !ok {
			return nil, fmt.Errorf("record %d: RTCP is not a Sender Report", rec.Index)
		}
		rec.SR = sr
	case KindTrunc:
		rec.Truncated = true
	default:
		return nil, fmt.Errorf("record %d: unknown kind %d", rec.Index, kind)
	}
	return rec, nil
}

// WriteRecord encodes one record; used by the generator and tests.
func WriteRecord(w io.Writer, kind uint8, payload []byte) error {
	if len(payload) > MaxPacketSize {
		return fmt.Errorf("%w: payload %d bytes", ErrLimit, len(payload))
	}
	var hdr [3]byte
	hdr[0] = kind
	binary.BigEndian.PutUint16(hdr[1:3], uint16(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
