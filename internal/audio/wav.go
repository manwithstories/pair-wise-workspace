package audio

import (
	"encoding/binary"
	"fmt"

	"mediad/internal/fault"
)

// EncodeWAV encodes a stereo/mono integer buffer as 16-bit PCM WAV.
func EncodeWAV(b Buffer) ([]byte, error) {
	if b.Channels < 1 || b.SampleRate <= 0 {
		return nil, fault.New(fault.InvalidArgument, "", -1, "invalid WAV format")
	}
	pcm := b.ToPCM16()
	data := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(v))
	}
	byteRate := uint32(b.SampleRate * b.Channels * 2)
	blockAlign := uint16(b.Channels * 2)
	riffSize := uint32(36 + len(data))
	out := make([]byte, 0, 44+len(data))
	out = append(out, "RIFF"...)
	out = binary.LittleEndian.AppendUint32(out, riffSize)
	out = append(out, "WAVE"...)
	out = append(out, "fmt "...)
	out = binary.LittleEndian.AppendUint32(out, 16)
	out = binary.LittleEndian.AppendUint16(out, 1)
	out = binary.LittleEndian.AppendUint16(out, uint16(b.Channels))
	out = binary.LittleEndian.AppendUint32(out, uint32(b.SampleRate))
	out = binary.LittleEndian.AppendUint32(out, byteRate)
	out = binary.LittleEndian.AppendUint16(out, blockAlign)
	out = binary.LittleEndian.AppendUint16(out, 16)
	out = append(out, "data"...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(data)))
	out = append(out, data...)
	return out, nil
}

// DecodeWAV decodes only the PCM16 WAV subset written by EncodeWAV.
func DecodeWAV(data []byte) (Buffer, error) {
	if len(data) < 44 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return Buffer{}, fault.New(fault.InvalidManifest, "", 0, "not a WAV file")
	}
	if string(data[12:16]) != "fmt " {
		return Buffer{}, fault.New(fault.InvalidManifest, "", 12, "missing fmt chunk")
	}
	format := binary.LittleEndian.Uint16(data[20:22])
	channels := int(binary.LittleEndian.Uint16(data[22:24]))
	rate := int(binary.LittleEndian.Uint32(data[24:28]))
	bits := binary.LittleEndian.Uint16(data[34:36])
	if format != 1 || bits != 16 {
		return Buffer{}, fault.New(fault.UnsupportedCodec, "", 20, "WAV supports PCM16 only")
	}
	// Fixture/export writer places data at offset 36? Actually data chunk starts 36, header 44.
	pos := 12
	var dataBytes []byte
	for pos+8 <= len(data) {
		id := string(data[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		body := pos + 8
		if body+size > len(data) {
			return Buffer{}, fault.New(fault.BoxTruncated, "", int64(len(data)), "WAV chunk %q truncated", id)
		}
		if id == "data" {
			dataBytes = data[body : body+size]
			break
		}
		pos = body + size + size%2
	}
	if dataBytes == nil {
		return Buffer{}, fault.New(fault.InvalidManifest, "", int64(len(data)), "WAV data chunk missing")
	}
	if len(dataBytes)%2 != 0 {
		return Buffer{}, fault.New(fault.MdatCorrupt, "", int64(len(data)), "WAV PCM length odd")
	}
	samples := make([]float64, len(dataBytes)/2)
	for i := range samples {
		v := int16(binary.LittleEndian.Uint16(dataBytes[2*i : 2*i+2]))
		samples[i] = FloatPCM(v)
	}
	if channels <= 0 || rate <= 0 {
		return Buffer{}, fmt.Errorf("invalid WAV audio parameters")
	}
	return Buffer{SampleRate: rate, Channels: channels, Samples: samples}, nil
}

// FloatPCM is exported for WAV decoding and tests.
func FloatPCM(v int16) float64 {
	if v == -32768 {
		return -1
	}
	return float64(v) / 32768
}
