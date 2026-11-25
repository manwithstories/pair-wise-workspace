// Package rtptrace parses local RTP companion records without reordering them.
package rtptrace

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"

	"github.com/pion/rtp"

	"bcast/internal/fault"
)

// Record is one JSON Lines companion record containing a base64 RTP packet.
type Record struct {
	RTPBase64 string `json:"rtp_b64"`
	Note      string `json:"note,omitempty"`
}

// Event is the observed order-preserving interpretation of one RTP packet.
type Event struct {
	Index             int    `json:"index"`
	SequenceNumber    uint16 `json:"sequence_number"`
	Timestamp         uint32 `json:"timestamp"`
	SSRC              uint32 `json:"ssrc"`
	Marker            bool   `json:"marker"`
	PayloadSize       int    `json:"payload_size"`
	DuplicateSequence bool   `json:"duplicate_sequence"`
	OutOfOrder        bool   `json:"out_of_order"`
	TimestampWrap     bool   `json:"timestamp_wrap"`
	Note              string `json:"note,omitempty"`
}

// Report contains events in file order and explicit anomaly counts.
type Report struct {
	Events             []Event `json:"events"`
	Packets            int     `json:"packets"`
	SequenceDuplicates int     `json:"sequence_duplicates"`
	OutOfOrder         int     `json:"out_of_order"`
	TimestampWraps     int     `json:"timestamp_wraps"`
}

// Parse reads JSON Lines RTP records. It never sorts packets: duplicates and
// wraparound are counted while original order remains available.
func Parse(r io.Reader) (*Report, error) {
	rep := &Report{Events: make([]Event, 0, 16)}
	seen := make(map[uint16]int)
	var lastSeq *uint16
	var lastTS *uint32

	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fault.E(fault.RTPInvalid, "rtptrace", "Parse.json", err).WithPath("line=" + itoa(lineNo))
		}
		raw, err := base64.StdEncoding.DecodeString(rec.RTPBase64)
		if err != nil {
			return nil, fault.E(fault.RTPInvalid, "rtptrace", "Parse.base64", err).WithPath("line=" + itoa(lineNo))
		}
		pkt := rtp.Packet{}
		if err := pkt.Unmarshal(raw); err != nil {
			return nil, fault.E(fault.RTPInvalid, "rtptrace", "Parse.rtp", err).WithPath("line=" + itoa(lineNo))
		}

		ev := Event{
			Index:          len(rep.Events),
			SequenceNumber: pkt.SequenceNumber,
			Timestamp:      pkt.Timestamp,
			SSRC:           pkt.SSRC,
			Marker:         pkt.Marker,
			PayloadSize:    len(pkt.Payload),
			Note:           rec.Note,
		}

		if first, ok := seen[pkt.SequenceNumber]; ok {
			ev.DuplicateSequence = true
			rep.SequenceDuplicates++
			_ = first
		} else {
			seen[pkt.SequenceNumber] = ev.Index
		}

		if lastSeq != nil && !ev.DuplicateSequence && pkt.SequenceNumber < *lastSeq {
			// A uint16 forward wrap (65534 -> 1) is not out-of-order.
			if !(*lastSeq >= 0xF000 && pkt.SequenceNumber <= 0x0FFF) {
				ev.OutOfOrder = true
				rep.OutOfOrder++
			}
		}
		lastSeq = &pkt.SequenceNumber

		if lastTS != nil {
			// Count only a high-to-low crossing consistent with uint32 wrap.
			if *lastTS >= 0xF0000000 && pkt.Timestamp <= 0x0FFFFFFF && pkt.Timestamp < *lastTS {
				ev.TimestampWrap = true
				rep.TimestampWraps++
			}
		}
		lastTS = &pkt.Timestamp

		rep.Events = append(rep.Events, ev)
		rep.Packets++
	}
	if err := s.Err(); err != nil {
		return nil, fault.E(fault.RTPInvalid, "rtptrace", "Parse.scan", err)
	}
	if rep.Packets == 0 {
		return nil, fault.E(fault.RTPInvalid, "rtptrace", "Parse", errors.New("no RTP records"))
	}
	return rep, nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
