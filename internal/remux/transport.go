package remux

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/pion/rtp"
)

// errTransportMismatch signals that a depacketized RTP header no longer
// matches the framing the pipeline produced.
var errTransportMismatch = errors.New("rtp header mismatch after marshal round trip")

// maxRTPPayload is the application payload cap per RTP packet. Block frames
// larger than this are fragmented across a packet sequence and reassembled
// before hashing, proving the archived payload survives transport framing.
const maxRTPPayload = 1200

// TransportResult records the RTP framing evidence for one block frame.
type TransportResult struct {
	Packets        int    `json:"packets"`
	MarkerPackets  int    `json:"marker_packets"`
	ReassembledSHA string `json:"reassembled_sha256"`
	RoundTripOK    bool   `json:"round_trip_ok"`
}

// VerifyOverRTP fragments frame into RTP packets, depacketizes them in
// order and confirms the reassembled bytes hash identically to the input.
// The SSRC/PT/sequence space is fixed so repeated runs are deterministic.
func VerifyOverRTP(frame []byte) (TransportResult, error) {
	ssrc := uint32(0xA1CC)
	payloadType := uint8(96)
	var packets []*rtp.Packet
	if len(frame) == 0 {
		packets = []*rtp.Packet{{
			Header:  rtp.Header{Version: 2, PayloadType: payloadType, SSRC: ssrc, Marker: true},
			Payload: nil,
		}}
	}
	for off, seq := 0, uint16(0); off < len(frame); seq++ {
		end := off + maxRTPPayload
		if end > len(frame) {
			end = len(frame)
		}
		chunk := append([]byte(nil), frame[off:end]...)
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    payloadType,
				SSRC:           ssrc,
				SequenceNumber: seq,
				Marker:         end == len(frame),
			},
			Payload: chunk,
		}
		packets = append(packets, pkt)
		off = end
	}

	var reassembled []byte
	markers := 0
	for _, pkt := range packets {
		raw, err := pkt.Marshal()
		if err != nil {
			return TransportResult{}, err
		}
		var decoded rtp.Packet
		if err = decoded.Unmarshal(raw); err != nil {
			return TransportResult{}, err
		}
		if decoded.SSRC != ssrc || decoded.PayloadType != payloadType {
			return TransportResult{}, errTransportMismatch
		}
		if decoded.Marker {
			markers++
		}
		reassembled = append(reassembled, decoded.Payload...)
	}
	origSum := sha256.Sum256(frame)
	reSum := sha256.Sum256(reassembled)
	ok := hex.EncodeToString(origSum[:]) == hex.EncodeToString(reSum[:])
	return TransportResult{
		Packets:        len(packets),
		MarkerPackets:  markers,
		ReassembledSHA: hex.EncodeToString(reSum[:]),
		RoundTripOK:    ok,
	}, nil
}
