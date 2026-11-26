package codec

import (
	"encoding/binary"
	"fmt"

	"tsproof/internal/adapter"
)

// PCM 常量：8 kHz 单声道、16-bit 小端。
const (
	PCMVersion    = 1
	PCMSampleRate = 8000
	PCMBytesFrame = 2
)

// PCMAdapter 解码带私有帧头的 PCM 音频块 PES。
type PCMAdapter struct{}

// NewPCM 创建 PCM 适配器。
func NewPCM() *PCMAdapter { return &PCMAdapter{} }

func (a *PCMAdapter) Name() string         { return "pcm" }
func (a *PCMAdapter) MajorVersion() uint16 { return PCMVersion }
func (a *PCMAdapter) StreamType() uint8    { return 0x81 }

// Decode 实现 adapter.CodecAdapter。音频块恒为可独立解码（Key=true）。
func (a *PCMAdapter) Decode(trackID uint16, frameIndex int, pts27 int64, pesPayload []byte) (*adapter.Block, error) {
	h, err := DecodeFrameHeader(pesPayload)
	if err != nil {
		return nil, err
	}
	raw := Payload(pesPayload)
	if len(raw)%PCMBytesFrame != 0 {
		return nil, fmt.Errorf("%w: pcm bytes not 16-bit aligned: %d", ErrFrameHeader, len(raw))
	}
	samples := len(raw) / PCMBytesFrame
	if samples != int(h.Length)/PCMBytesFrame {
		return nil, fmt.Errorf("%w: sample count header=%d actual=%d",
			ErrFrameHeader, h.Length/PCMBytesFrame, samples)
	}
	return &adapter.Block{
		TrackID: trackID,
		Index:   frameIndex,
		PTS27:   pts27,
		Key:     true,
		Kind:    adapter.KindAudio,
		Payload: raw,
		Samples: samples,
	}, nil
}

// EncodePCMBlock 是生成器侧辅助：把 16-bit 样本与帧头打包成 PES 负载。
func EncodePCMBlock(index uint32, samples []int16) []byte {
	raw := make([]byte, len(samples)*PCMBytesFrame)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(s))
	}
	buf := make([]byte, FrameHeaderLen+len(raw))
	EncodeFrameHeader(buf, Header{
		Version: 1,
		Index:   index,
		Key:     true,
		Length:  uint32(len(raw)),
	})
	copy(buf[FrameHeaderLen:], raw)
	return buf
}
