// Package sample generates deterministic in-repository Matroska fixtures.
package sample

import (
	"math/rand"
)

// SilencePacket returns a valid one-packet 40 ms Opus silence frame. Pion
// decodes configuration 10 to 1920 samples at 48 kHz, i.e. two 20 ms windows.
func SilencePacket() []byte {
	return []byte{10 << 3}
}

// StrongVoicePacket returns a deterministic CELT packet with high RMS, used
// immediately after silence to exercise hysteresis exit.
func StrongVoicePacket() []byte {
	pkt := make([]byte, 61)
	pkt[0] = 27 << 3
	for i := 1; i < len(pkt); i++ {
		pkt[i] = byte((i*37 + 27*11) % 251)
		if pkt[i] == 0 {
			pkt[i] = 1
		}
	}
	return pkt
}

// VoicePacket returns a deterministic, varied Opus packet for cluster i.
func VoicePacket(seed int64, i int) []byte {
	if i == 11 {
		return StrongVoicePacket()
	}
	r := rand.New(rand.NewSource(seed*1000 + int64(i)*7919 + 17))
	pkt := make([]byte, 48)
	pkt[0] = 27 << 3
	_, _ = r.Read(pkt[1:])
	return pkt
}

// DuplicateVoicePacket is the intentionally repeated media payload.
func DuplicateVoicePacket(seed int64) []byte {
	return VoicePacket(seed, 30)
}
