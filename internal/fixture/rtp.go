package fixture

import (
	"encoding/binary"
	"math/rand"
)

// RTPConfig describes a deterministic local RTP timing capture.
type RTPConfig struct {
	SSRC       uint32
	ClockRate  uint32
	BaseSeq    uint16
	BaseTS     uint32
	PacketRate int // packets per simulated second
	Count      int
	PayloadLen int
}

// DefaultRTPConfig returns a 50 pps, 90 kHz video-clock capture.
func DefaultRTPConfig(seed int64) RTPConfig {
	return RTPConfig{
		SSRC:       0x52_56_57_31,
		ClockRate:  90000,
		BaseSeq:    uint16(seed*131 + 17),
		BaseTS:     uint32(seed*1_000_003) & 0x7FFFFFFF,
		PacketRate: 50,
		Count:      40,
		PayloadLen: 12,
	}
}

// CaptureRTP marshals Count RTP packets with monotonically increasing
// sequence numbers and clock-rate-scaled timestamps. The payload is
// deterministic from seed, including a marker every tenth packet.
func CaptureRTP(seed int64, count int) [][]byte {
	cfg := DefaultRTPConfig(seed)
	if count > 0 {
		cfg.Count = count
	}
	rng := rand.New(rand.NewSource(seed + 99))
	out := make([][]byte, 0, cfg.Count)
	step := cfg.ClockRate / uint32(cfg.PacketRate)
	for i := 0; i < cfg.Count; i++ {
		var hdr [12]byte
		hdr[0] = 0x80 // V2, no padding/extension/CSRC
		if i%10 == 9 {
			hdr[1] = 0x60 // marker + PT 96
		} else {
			hdr[1] = 0x60
		}
		seq := cfg.BaseSeq + uint16(i)
		ts := cfg.BaseTS + uint32(i)*step
		binary.BigEndian.PutUint16(hdr[2:], seq)
		binary.BigEndian.PutUint32(hdr[4:], ts)
		binary.BigEndian.PutUint32(hdr[8:], cfg.SSRC)
		pkt := make([]byte, 12+cfg.PayloadLen)
		copy(pkt, hdr[:])
		for j := 0; j < cfg.PayloadLen; j++ {
			pkt[12+j] = byte(rng.Intn(256))
		}
		out = append(out, pkt)
	}
	return out
}

// CaptureRTPWithConfig is the explicit-configuration variant.
func CaptureRTPWithConfig(cfg RTPConfig, seed int64) [][]byte {
	rng := rand.New(rand.NewSource(seed + 99))
	out := make([][]byte, 0, cfg.Count)
	step := cfg.ClockRate / uint32(cfg.PacketRate)
	for i := 0; i < cfg.Count; i++ {
		var hdr [12]byte
		hdr[0] = 0x80
		hdr[1] = 0x60
		binary.BigEndian.PutUint16(hdr[2:], cfg.BaseSeq+uint16(i))
		binary.BigEndian.PutUint32(hdr[4:], cfg.BaseTS+uint32(i)*step)
		binary.BigEndian.PutUint32(hdr[8:], cfg.SSRC)
		pkt := make([]byte, 12+cfg.PayloadLen)
		copy(pkt, hdr[:])
		for j := 0; j < cfg.PayloadLen; j++ {
			pkt[12+j] = byte(rng.Intn(256))
		}
		out = append(out, pkt)
	}
	return out
}
