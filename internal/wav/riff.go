package wav

import (
	"bytes"
	"encoding/binary"
)

// riff.go writes the minimal RIFF/WAVE container used by fixtures. The
// writer is hand-rolled rather than using wav.Encoder so the rsid side
// chunk can sit between fmt and data and the declared sizes survive a
// tail truncation (which is how truncation damage is simulated).

func writeHeader(out *bytes.Buffer, cfg GenerateConfig, dataLen int, rsid []byte) {
	blockAlign := cfg.Channels * int(cfg.BitDepth) / 8
	fmtSize := 16
	rsidLen := len(rsid) + len(rsid)%2 // word-aligned side chunk
	pcmLen := dataLen + dataLen%2
	riffLen := 4 + (8+fmtSize) + (8+rsidLen) + (8+pcmLen)
	binary.Write(out, binary.LittleEndian, [4]byte{'R', 'I', 'F', 'F'})
	binary.Write(out, binary.LittleEndian, uint32(riffLen))
	binary.Write(out, binary.LittleEndian, [4]byte{'W', 'A', 'V', 'E'})

	binary.Write(out, binary.LittleEndian, [4]byte{'f', 'm', 't', ' '})
	binary.Write(out, binary.LittleEndian, uint32(fmtSize))
	binary.Write(out, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(out, binary.LittleEndian, uint16(cfg.Channels))
	binary.Write(out, binary.LittleEndian, uint32(cfg.SampleRate))
	binary.Write(out, binary.LittleEndian, uint32(int(cfg.SampleRate)*blockAlign))
	binary.Write(out, binary.LittleEndian, uint16(blockAlign))
	binary.Write(out, binary.LittleEndian, uint16(cfg.BitDepth))
}

func writeSideChunk(out *bytes.Buffer, id [4]byte, payload []byte) {
	binary.Write(out, binary.LittleEndian, id)
	binary.Write(out, binary.LittleEndian, uint32(len(payload)))
	out.Write(payload)
	if len(payload)%2 == 1 {
		out.WriteByte(0)
	}
}

func writeDataChunk(out *bytes.Buffer, pcm []byte) {
	binary.Write(out, binary.LittleEndian, [4]byte{'d', 'a', 't', 'a'})
	binary.Write(out, binary.LittleEndian, uint32(len(pcm)))
	out.Write(pcm)
	if len(pcm)%2 == 1 {
		out.WriteByte(0) // RIFF word alignment
	}
}
