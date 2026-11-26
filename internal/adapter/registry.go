// Package adapter 定义版本化的容器与编解码适配器接口及注册表。
// 不支持的适配器版本必须显式失败，禁止静默回退。
package adapter

import (
	"errors"
	"fmt"
)

// 当前适配器契约主版本。解析器遇到更高主版本即拒绝。
const ContractMajor = 1

// Block 是适配器解码后的标准化媒体块（帧或音频块）。
type Block struct {
	TrackID   uint16
	Index     int          // 轨道内单调序号
	PTS27     int64        // 27 MHz 展开前的原始 PTS（由时钟层展开）
	Key       bool         // 关键帧（音频块恒为 true，可独立解码）
	Kind      BlockKind
	Payload   []byte       // 解码后的原始字节（PCM 样本或 JPEG 仍为 JPEG 字节）
	Width     int          // 图像块尺寸；音频为 0
	Height    int
	Samples   int          // 音频样本数；图像为 0
	Untrusted bool         // 来自带缺口 PES 的块
}

// BlockKind 区分图像帧与音频块。
type BlockKind uint8

const (
	KindUnknown BlockKind = iota
	KindImage
	KindAudio
)

// CodecAdapter 是编解码适配器的版本化接口。
type CodecAdapter interface {
	// Name 返回适配器名（如 "jpeg"、"pcm"）。
	Name() string
	// MajorVersion 返回实现的契约主版本。
	MajorVersion() uint16
	// StreamType 返回写入 PMT 的私有 stream_type。
	StreamType() uint8
	// Decode 把 PES 负载解码为标准块。frameIndex 由调用方按轨道维护。
	Decode(trackID uint16, frameIndex int, pts27 int64, pesPayload []byte) (*Block, error)
}

// ErrUnknownAdapter 表示 stream_type 没有注册适配器。
var ErrUnknownAdapter = errors.New("adapter: unknown stream_type")

// ErrVersion 表示适配器契约版本不受支持。
var ErrVersion = errors.New("adapter: unsupported contract version")

// Registry 是 stream_type → 适配器的版本化查找表。
type Registry struct {
	adapters map[uint8]CodecAdapter
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{adapters: map[uint8]CodecAdapter{}}
}

// Register 注册适配器；重复 stream_type 或版本不被支持时返回错误。
func (r *Registry) Register(a CodecAdapter) error {
	if a.MajorVersion() != ContractMajor {
		return fmt.Errorf("%w: %s major=%d want=%d",
			ErrVersion, a.Name(), a.MajorVersion(), ContractMajor)
	}
	if _, exists := r.adapters[a.StreamType()]; exists {
		return fmt.Errorf("adapter: stream_type 0x%02x already registered", a.StreamType())
	}
	r.adapters[a.StreamType()] = a
	return nil
}

// Lookup 按 stream_type 查找适配器，未注册返回 ErrUnknownAdapter。
func (r *Registry) Lookup(streamType uint8) (CodecAdapter, error) {
	a, ok := r.adapters[streamType]
	if !ok {
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnknownAdapter, streamType)
	}
	return a, nil
}

// MustRegister 注册失败时 panic，仅供进程启动期装配使用。
func (r *Registry) MustRegister(a CodecAdapter) {
	if err := r.Register(a); err != nil {
		panic(err)
	}
}

// StreamTypes 返回已注册的 stream_type 升序列表，保证确定性。
func (r *Registry) StreamTypes() []uint8 {
	out := make([]uint8, 0, len(r.adapters))
	for st := range r.adapters {
		out = append(out, st)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
