// Package rtpprobe performs optional RTP packet-structure validation for
// archived samples. It does not transport anything: it frames a sample into
// RTP packets with pion/rtp, marshals them, parses them back, and checks that
// the payload survives the round trip unchanged.
//
// The check is optional because most archives are stored, not streamed. When
// enabled it gives a second, codec-agnostic integrity signal: the packet
// headers must be structurally valid for the sample sizes the remuxer emitted.
package rtpprobe

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"

	"archivecheck.local/archivecheck/internal/fault"
)

// ErrInvalid marks a packet whose structure or payload failed validation.
var ErrInvalid = errors.New("rtp: invalid packet")

// DefaultMTU is the RTP payload budget per packet, below a typical 1500-byte
// Ethernet MTU once IP, UDP and RTP headers are accounted for.
const DefaultMTU = 1200

// Result reports one validation pass.
type Result struct {
	Samples int
	Packets int
	// MaxPayload is the largest payload carried by a single packet.
	MaxPayload int
}

// Validate frames each sample into RTP packets and checks the round trip.
// clockRate is used for the RTP timestamp field; it does not affect payload
// validation. A nil payload is rejected so a caller cannot accidentally
// "validate" an empty sample.
func Validate(samples [][]byte, payloadType uint8, clockRate uint32, mtu int) (Result, error) {
	if len(samples) == 0 {
		return Result{}, nil
	}
	if mtu <= 0 {
		mtu = DefaultMTU
	}
	var res Result
	var seq uint16
	var ts uint32
	for i, data := range samples {
		if len(data) == 0 {
			return res, fault.New(fault.StageVerify, fmt.Errorf("%w: sample %d is empty", ErrInvalid, i))
		}
		res.Samples++
		for off := 0; off < len(data); off += mtu {
			end := off + mtu
			if end > len(data) {
				end = len(data)
			}
			chunk := data[off:end]
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    payloadType,
					SequenceNumber: seq,
					Timestamp:      ts,
					SSRC:           0x41524348, // "ARCH"
					Marker:         end == len(data),
				},
				Payload: chunk,
			}
			raw, err := pkt.Marshal()
			if err != nil {
				return res, fault.New(fault.StageVerify, fmt.Errorf("%w: %v", ErrInvalid, err))
			}
			var parsed rtp.Packet
			if err := parsed.Unmarshal(raw); err != nil {
				return res, fault.New(fault.StageVerify, fmt.Errorf("%w: %v", ErrInvalid, err))
			}
			if parsed.Version != 2 {
				return res, fault.New(fault.StageVerify, fmt.Errorf("%w: version %d", ErrInvalid, parsed.Version))
			}
			if len(parsed.Payload) != len(chunk) {
				return res, fault.New(fault.StageVerify,
					fmt.Errorf("%w: payload length %d != %d", ErrInvalid, len(parsed.Payload), len(chunk)))
			}
			for j := range chunk {
				if parsed.Payload[j] != chunk[j] {
					return res, fault.New(fault.StageVerify,
						fmt.Errorf("%w: payload byte %d differs after round trip", ErrInvalid, j))
				}
			}
			if len(chunk) > res.MaxPayload {
				res.MaxPayload = len(chunk)
			}
			res.Packets++
			seq++
		}
		// One frame per timestamp step, mirroring a simple packetizer.
		ts += clockRate / 50
	}
	return res, nil
}
