package main

import (
	"encoding/binary"
	"os"

	"archivemedia/internal/audio"
)

// wavHeaderLen 与 pipeline 输出的 44 字节标准 PCM WAV 头一致。
const wavHeaderLen = 44

// readWAVStereo 读取 16-bit 立体声 WAV 为交错浮点缓冲。
func readWAVStereo(path string) (*audio.Buffer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < wavHeaderLen {
		return nil, os.ErrInvalid
	}
	pcm := data[wavHeaderLen:]
	frames := len(pcm) / 4 // 立体声 16-bit = 4 字节/帧
	out := make([]float32, frames*2)
	for i := 0; i < frames; i++ {
		l := int16(binary.LittleEndian.Uint16(pcm[i*4:]))
		r := int16(binary.LittleEndian.Uint16(pcm[i*4+2:]))
		out[i*2] = float32(l) / 32767.0
		out[i*2+1] = float32(r) / 32767.0
	}
	return &audio.Buffer{Samples: out, Frames: frames}, nil
}
