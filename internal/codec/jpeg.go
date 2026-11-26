package codec

import (
	"bytes"
	"fmt"

	"tsproof/internal/adapter"
)

// JPEGVersion 是 JPEG 适配器实现的契约主版本。
const JPEGVersion = 1

// JPEGAdapter 解码带私有帧头的 JPEG 帧 PES。
// 它不在解码阶段展开图片（渲染阶段才用 imaging 解码），
// 这里负责帧头校验、尺寸限制与块标准化。
type JPEGAdapter struct{}

// NewJPEG 创建 JPEG 适配器。
func NewJPEG() *JPEGAdapter { return &JPEGAdapter{} }

func (a *JPEGAdapter) Name() string          { return "jpeg" }
func (a *JPEGAdapter) MajorVersion() uint16  { return JPEGVersion }
func (a *JPEGAdapter) StreamType() uint8     { return 0x80 }

// Decode 实现 adapter.CodecAdapter。
func (a *JPEGAdapter) Decode(trackID uint16, frameIndex int, pts27 int64, pesPayload []byte) (*adapter.Block, error) {
	h, err := DecodeFrameHeader(pesPayload)
	if err != nil {
		return nil, err
	}
	jpegBytes := Payload(pesPayload)
	// JPEG 以 SOI 0xFFD8 开头、EOI 0xFFD9 结尾，做轻量自描述校验。
	if len(jpegBytes) < 4 || !bytes.HasPrefix(jpegBytes, []byte{0xFF, 0xD8}) ||
		!bytes.HasSuffix(jpegBytes, []byte{0xFF, 0xD9}) {
		return nil, fmt.Errorf("%w: not a JPEG payload", ErrFrameHeader)
	}
	return &adapter.Block{
		TrackID: trackID,
		Index:   frameIndex,
		PTS27:   pts27,
		Key:     h.Key,
		Kind:    adapter.KindImage,
		Payload: jpegBytes,
		Width:   int(h.Width),
		Height:  int(h.Height),
	}, nil
}

// EncodeJPEGBlock 是生成器侧辅助：把 JPEG 字节与帧头打包成 PES 负载。
func EncodeJPEGBlock(index uint32, key bool, width, height int, jpegBytes []byte) []byte {
	buf := make([]byte, FrameHeaderLen+len(jpegBytes))
	EncodeFrameHeader(buf, Header{
		Version: 1,
		Index:   index,
		Key:     key,
		Width:   uint32(width),
		Height:  uint32(height),
		Length:  uint32(len(jpegBytes)),
	})
	copy(buf[FrameHeaderLen:], jpegBytes)
	return buf
}
