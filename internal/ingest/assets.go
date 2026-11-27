package ingest

import (
	"encoding/binary"
	"fmt"
)

// encodedAudio is one real Opus packet plus the duration its bitstream
// declares, so tests can compare parser output with the encoder.
type encodedAudio struct {
	Data    []byte
	Samples int // samples per channel at 48 kHz
}

// encodedVideo is one H.264 access unit in AVCC framing (4-byte big endian
// NAL lengths), which is also what Matroska stores.
type encodedVideo struct {
	Data     []byte
	Keyframe bool
	NALTypes []byte
}

// mediaAssets holds the deterministic codec payloads of a fixture together
// with the stream parameters and extradata a muxer needs.
type mediaAssets struct {
	Audio      []encodedAudio
	Video      []encodedVideo
	AudioExtra []byte
	VideoExtra []byte
}

// annexBToAVCC rewrites start-code framed NAL units as 4-byte length
// prefixed units and returns the NAL header types seen.
func annexBToAVCC(b []byte) ([]byte, []byte, error) {
	var out []byte
	var types []byte
	i := 0
	for {
		start, scLen := findStartCode(b, i)
		if start < 0 {
			break
		}
		body := start + scLen
		next, _ := findStartCode(b, body)
		if next < 0 {
			next = len(b)
		}
		nal := b[body:next]
		if len(nal) == 0 {
			i = next
			continue
		}
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(nal)))
		out = append(out, l[:]...)
		out = append(out, nal...)
		types = append(types, nal[0]&0x1f)
		i = next
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("no NAL units in %d byte access unit", len(b))
	}
	return out, types, nil
}

func findStartCode(b []byte, from int) (int, int) {
	for i := from; i+3 <= len(b); i++ {
		if b[i] != 0 || b[i+1] != 0 {
			continue
		}
		if b[i+2] == 1 {
			return i, 3
		}
		if b[i+2] == 0 && i+4 <= len(b) && b[i+3] == 1 {
			return i, 4
		}
	}
	return -1, 0
}
