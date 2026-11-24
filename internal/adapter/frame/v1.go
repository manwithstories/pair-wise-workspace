// Package frame implements the versioned frame-sequence adapter (v1).
// Delivery format: each frame is a binary record carried as the payload
// of one RTP packet (pion/rtp), giving a 32-bit capture clock and packet
// boundaries. The adapter validates sequence numbers, timestamps and
// SHA-256 checksums and converts frames onto the unified time base.
package frame

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"

	"github.com/pion/rtp"

	"media-check/internal/model"
)

const (
	AdapterID = "frame-seq"
	Version   = "v1"
)

// Adapter is the frame-sequence adapter v1.
type Adapter struct{}

func (Adapter) ID() string      { return AdapterID }
func (Adapter) Version() string { return Version }

// Record is one delivered frame with its declared metadata and checksum.
type Record struct {
	Seq       uint32
	CaptureTS uint32
	Keyframe  bool
	Width     uint16
	Height    uint16
	Color     model.ColorMeta
	Payload   []byte
	Sum       [32]byte // SHA-256 of Payload, declared by the supplier
}

// GenOptions parameterises deterministic sample generation.
type GenOptions struct {
	Count       int
	StartSeq    uint32
	StartTS     uint32
	TSDelta     uint32
	GOPSize     int
	Width       uint16
	Height      uint16
	PayloadSize int
	Seed        int64
	Color       model.ColorMeta
}

// Generate builds a deterministic, well-formed frame sequence.
func Generate(o GenOptions) []Record {
	rng := rand.New(rand.NewSource(o.Seed))
	recs := make([]Record, 0, o.Count)
	for i := 0; i < o.Count; i++ {
		payload := make([]byte, o.PayloadSize)
		rng.Read(payload)
		// embed the sequence number so payloads differ predictably
		binary.BigEndian.PutUint32(payload[:4], o.StartSeq+uint32(i))
		recs = append(recs, Record{
			Seq:       o.StartSeq + uint32(i),
			CaptureTS: o.StartTS + uint32(i)*o.TSDelta,
			Keyframe:  o.GOPSize > 0 && i%o.GOPSize == 0,
			Width:     o.Width,
			Height:    o.Height,
			Color:     o.Color,
			Payload:   payload,
			Sum:       sha256.Sum256(payload),
		})
	}
	return recs
}

// recordHeaderLen is seq|ts|flags|width|height|colorLen = 4+4+1+2+2+2.
const recordHeaderLen = 15

// Marshal serialises a record deterministically (header, colour tags,
// payload, declared checksum).
func (r Record) Marshal() []byte {
	color := r.Color.Primaries + "|" + r.Color.Transfer + "|" + r.Color.Matrix
	buf := make([]byte, 0, recordHeaderLen+len(color)+len(r.Payload)+sha256.Size)
	var hdr [recordHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[0:4], r.Seq)
	binary.BigEndian.PutUint32(hdr[4:8], r.CaptureTS)
	if r.Keyframe {
		hdr[8] = 1
	}
	binary.BigEndian.PutUint16(hdr[9:11], r.Width)
	binary.BigEndian.PutUint16(hdr[11:13], r.Height)
	binary.BigEndian.PutUint16(hdr[13:15], uint16(len(color)))
	buf = append(buf, hdr[:]...)
	buf = append(buf, color...)
	buf = append(buf, r.Payload...)
	buf = append(buf, r.Sum[:]...)
	return buf
}

// ParseRecord decodes one record, validating all lengths before reading.
func ParseRecord(b []byte) (Record, error) {
	var r Record
	if len(b) < recordHeaderLen+sha256.Size {
		return r, fmt.Errorf("record too short: %d bytes", len(b))
	}
	r.Seq = binary.BigEndian.Uint32(b[0:4])
	r.CaptureTS = binary.BigEndian.Uint32(b[4:8])
	r.Keyframe = b[8] != 0
	r.Width = binary.BigEndian.Uint16(b[9:11])
	r.Height = binary.BigEndian.Uint16(b[11:13])
	clen := int(binary.BigEndian.Uint16(b[13:15]))
	if len(b) < recordHeaderLen+clen+sha256.Size {
		return r, fmt.Errorf("record colour field overruns buffer: %d", clen)
	}
	color := string(b[recordHeaderLen : recordHeaderLen+clen])
	parts := split3(color)
	r.Color = model.ColorMeta{Primaries: parts[0], Transfer: parts[1], Matrix: parts[2]}
	payloadEnd := len(b) - sha256.Size
	r.Payload = append([]byte(nil), b[recordHeaderLen+clen:payloadEnd]...)
	copy(r.Sum[:], b[payloadEnd:])
	return r, nil
}

func split3(s string) [3]string {
	var out [3]string
	start, idx := 0, 0
	for i := 0; i < len(s) && idx < 2; i++ {
		if s[i] == '|' {
			out[idx] = s[start:i]
			idx++
			start = i + 1
		}
	}
	out[idx] = s[start:]
	return out
}

// --- RTP transport (pion/rtp): packet boundaries + 32-bit capture clock ---

// ToPackets wraps each record in one RTP packet. The RTP timestamp is the
// record capture clock; the RTP sequence number is supplied by the caller
// so tests and demos can model 16-bit wrap independently.
func ToPackets(recs []Record, ssrc uint32, rtpSeqStart uint16) ([][]byte, error) {
	out := make([][]byte, 0, len(recs))
	for i, r := range recs {
		pkt := rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				SequenceNumber: rtpSeqStart + uint16(i),
				Timestamp:      r.CaptureTS,
				SSRC:           ssrc,
				Marker:         r.Keyframe,
			},
			Payload: r.Marshal(),
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return nil, fmt.Errorf("marshal rtp packet %d: %w", i, err)
		}
		out = append(out, raw)
	}
	return out, nil
}

// IngestResult carries the frames that passed validation plus every
// located fault. Frames that fail checksum or structure checks are
// excluded but their positions are reported through Faults.
type IngestResult struct {
	Frames []model.Frame
	Faults model.Faults
}

// Ingest decodes RTP packets, validates each record (structure, sequence
// continuity, timestamp monotonicity across wrap, SHA-256) and unwraps
// the capture clock onto the unified time base. segmentOf maps a
// sequence number to its segment index for fault localisation.
func (a Adapter) Ingest(rawPackets [][]byte, segmentOf func(seq uint32) int) IngestResult {
	res := IngestResult{}
	var unw model.Unwrapper
	var expectSeq uint32
	var haveExpect bool
	var lastTime int64
	var haveLast bool

	for i, raw := range rawPackets {
		var pkt rtp.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			res.Faults = append(res.Faults, a.fault(model.FaultBoxTruncated, -1,
				fmt.Sprintf("rtp packet %d undecodable: %v", i, err)))
			continue
		}
		rec, err := ParseRecord(pkt.Payload)
		seg := -1
		if err == nil && segmentOf != nil {
			seg = segmentOf(rec.Seq)
		}
		if err != nil {
			res.Faults = append(res.Faults, a.fault(model.FaultBoxTruncated, seg,
				fmt.Sprintf("record %d undecodable: %v", i, err)))
			continue
		}
		if pkt.Header.Timestamp != rec.CaptureTS {
			res.Faults = append(res.Faults, a.fault(model.FaultTimestampRegression, seg,
				fmt.Sprintf("seq=%d rtp ts %d != record ts %d", rec.Seq, pkt.Header.Timestamp, rec.CaptureTS)))
			continue
		}
		if haveExpect && rec.Seq == expectSeq-1 {
			res.Faults = append(res.Faults, a.fault(model.FaultSeqDuplicate, seg,
				fmt.Sprintf("duplicate seq=%d", rec.Seq)))
			continue
		}
		if haveExpect && rec.Seq != expectSeq {
			res.Faults = append(res.Faults, a.fault(model.FaultSeqGap, seg,
				fmt.Sprintf("expected seq=%d got %d", expectSeq, rec.Seq)))
			// resynchronise: the gap is located, keep ingesting
		}
		if sha256.Sum256(rec.Payload) != rec.Sum {
			res.Faults = append(res.Faults, a.fault(model.FaultHashMismatch, seg,
				fmt.Sprintf("seq=%d checksum mismatch", rec.Seq)))
			// the packet was delivered, so its sequence position is
			// consumed even though the payload is rejected
			expectSeq = rec.Seq + 1
			haveExpect = true
			continue
		}
		t := unw.Next(rec.CaptureTS)
		if haveLast && t < lastTime {
			res.Faults = append(res.Faults, a.fault(model.FaultTimestampRegression, seg,
				fmt.Sprintf("seq=%d time %d regresses below %d", rec.Seq, t, lastTime)))
			continue
		}
		expectSeq = rec.Seq + 1
		haveExpect = true
		lastTime = t
		haveLast = true
		res.Frames = append(res.Frames, model.Frame{
			Seq: rec.Seq, CaptureTS: rec.CaptureTS, Time: t,
			Keyframe: rec.Keyframe, Width: rec.Width, Height: rec.Height,
			Color: rec.Color, Payload: rec.Payload, Hash: rec.Sum,
		})
	}
	return res
}

func (a Adapter) fault(code model.FaultCode, seg int, detail string) model.Fault {
	return model.Fault{Code: code, Adapter: a.ID() + "@" + a.Version(), Segment: seg, Detail: detail}
}
