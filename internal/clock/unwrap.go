package clock

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/pion/rtp"

	"review/internal/issues"
)

// RTPRecord is one expanded RTP timestamp.
type RTPRecord struct {
	Seq       int    // packet index in the stream
	Raw       uint32 // raw 32-bit RTP timestamp
	Unwrapped uint64 // monotonic 64-bit expansion
	Wrapped   bool   // a 32-bit wrap happened at this packet
}

// UnwrapRTP expands 32-bit RTP timestamps into a monotonic 64-bit series.
// expectedDelta is the nominal per-frame step in RTP clock units. Duplicate,
// backward and gapped timestamps are classified into the unified error
// model; a backward glitch does not disturb the reference of later packets.
func UnwrapRTP(raw []uint32, expectedDelta uint32) ([]RTPRecord, []issues.Issue) {
	recs := make([]RTPRecord, 0, len(raw))
	var iss []issues.Issue
	var high, prevUnwrap uint64
	var prevRaw uint32
	for i, r := range raw {
		rec := RTPRecord{Seq: i, Raw: r}
		if i == 0 {
			rec.Unwrapped = uint64(r)
		} else if d := int32(r - prevRaw); d == 0 {
			iss = append(iss, issues.Issue{Kind: issues.RTPDuplicate, Line: i,
				Raw: fmt.Sprintf("%d", r), Msg: "duplicate RTP timestamp"})
			rec.Unwrapped = prevUnwrap
		} else if d > 0 {
			if r < prevRaw { // 32-bit wrap-around
				high += 1 << 32
				rec.Wrapped = true
			}
			rec.Unwrapped = high + uint64(r)
			if expectedDelta > 0 && uint32(d) > expectedDelta*3/2 {
				iss = append(iss, issues.Issue{Kind: issues.RTPGap, Line: i,
					Raw: fmt.Sprintf("%d", r),
					Msg: fmt.Sprintf("timestamp gap of %d units (expect ~%d)", d, expectedDelta)})
			}
		} else { // d < 0: backward jump, treat as an isolated glitch
			iss = append(iss, issues.Issue{Kind: issues.RTPBackward, Line: i,
				Raw: fmt.Sprintf("%d", r), Msg: "timestamp moved backward"})
			rec.Unwrapped = high + uint64(r)
			recs = append(recs, rec)
			continue // keep prevRaw/prevUnwrap as the last good reference
		}
		prevRaw, prevUnwrap = r, rec.Unwrapped
		recs = append(recs, rec)
	}
	return recs, iss
}

// ReadRTPStream reads a stream of 2-byte big-endian length-prefixed RTP
// packets (RTP-over-TCP framing) and returns the raw 32-bit timestamps.
func ReadRTPStream(path string) ([]uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []uint32
	var hdr [2]byte
	for {
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, fmt.Errorf("rtp stream: %w", err)
		}
		buf := make([]byte, binary.BigEndian.Uint16(hdr[:]))
		if _, err := io.ReadFull(f, buf); err != nil {
			return out, fmt.Errorf("rtp packet %d truncated: %w", len(out), err)
		}
		var p rtp.Packet
		if err := p.Unmarshal(buf); err != nil {
			return out, fmt.Errorf("rtp packet %d: %w", len(out), err)
		}
		out = append(out, p.Timestamp)
	}
}
