// Package source generates deterministic local length-prefixed RTP/RTCP feeds.
package source

import (
	"encoding/binary"
	"math"

	"github.com/pion/rtp"
)

// ToneL16 creates big-endian signed L16 samples for a mono/stereo sine tone.
// A small harmonic component makes loudness gating stable while remaining
// deterministic.
func ToneL16(frames, channels, rate int, amplitude, freq float64) []byte {
	samples := make([]int16, frames*channels)
	for f := 0; f < frames; f++ {
		t := float64(f) / float64(rate)
		v := amplitude*math.Sin(2*math.Pi*freq*t) +
			amplitude*0.15*math.Sin(2*math.Pi*2*freq*t)
		if v > 0.98 {
			v = 0.98
		}
		if v < -0.98 {
			v = -0.98
		}
		s := int16(math.Round(v * 32767))
		for ch := 0; ch < channels; ch++ {
			samples[f*channels+ch] = s
		}
	}
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.BigEndian.PutUint16(out[2*i:2*i+2], uint16(s))
	}
	return out
}

// FramePrefix prepends a four-byte big-endian datagram length.
func FramePrefix(datagram []byte) []byte {
	prefix := make([]byte, 4)
	binary.BigEndian.PutUint32(prefix, uint32(len(datagram)))
	return append(prefix, datagram...)
}

// RTPDatagram marshals one L16 RTP packet.
func RTPDatagram(ssrc uint32, pt uint8, seq uint16, ts uint32, payload []byte) ([]byte, error) {
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    pt,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
	return pkt.Marshal()
}

// SenderReport builds a 28-byte empty-report-block RTCP SR.
func SenderReport(ssrc, rtpTS uint32, ntpSeconds uint32) []byte {
	b := make([]byte, 28)
	b[0] = 0x80 // V=2, RC=0
	b[1] = 200
	binary.BigEndian.PutUint16(b[2:4], 6) // length in 32-bit units minus one
	binary.BigEndian.PutUint32(b[4:8], ssrc)
	binary.BigEndian.PutUint32(b[8:12], ntpSeconds)
	binary.BigEndian.PutUint32(b[12:16], 0)
	binary.BigEndian.PutUint32(b[16:20], rtpTS)
	return b
}

// BadCSRCDatagram returns an RTP frame whose CC claims an impossible header.
func BadCSRCDatagram() []byte {
	b := make([]byte, 24)
	b[0] = 0x8f // V=2, CC=15
	b[1] = 96
	binary.BigEndian.PutUint16(b[2:4], 1)
	binary.BigEndian.PutUint32(b[8:12], 0x11111111)
	return b
}

// BadExtensionDatagram returns an RTP frame whose extension length is cut off.
func BadExtensionDatagram() []byte {
	b := make([]byte, 20)
	b[0] = 0x90 // V=2, X=1
	b[1] = 96
	binary.BigEndian.PutUint16(b[14:16], 10) // claims 40 extension bytes
	return b
}
