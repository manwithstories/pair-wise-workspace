package recovery

import (
	"encoding/binary"

	"mediad/internal/audio"
)

func encodePCM16LE(samples []int16) []byte {
	out := make([]byte, 2*len(samples))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

func decodePCM16LE(data []byte) []int16 {
	out := make([]int16, len(data)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(data[2*i : 2*i+2]))
	}
	return out
}

// processPCM applies the canonical layout and sample-rate conversion.
func processPCM(samples []int16, p Params) ([]int16, error) {
	buf, err := audio.PCM16ToBuffer(samples, p.SourceRate, p.SourceChannels)
	if err != nil {
		return nil, err
	}
	down, err := audio.Downmix(buf, p.Layout)
	if err != nil {
		return nil, err
	}
	out, err := audio.Resample(down, p.Rate)
	if err != nil {
		return nil, err
	}
	return out.ToPCM16(), nil
}
