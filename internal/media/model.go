// Package media 定义跨模块共享的统一帧模型、时间基准与错误类型。
package media

import "fmt"

// 协议常量：MFS/1 与 MTA/1 共用的记录线格式。
const (
	WireVersion1 byte = 1 // 唯一受支持的记录版本

	// 记录头固定长度（不含载荷与尾部校验）。
	HeaderLen = 42
	// 记录尾部：SHA-256(32) + CRC32C(4)。
	TailLen = 36
	// 单条记录允许的最大载荷，防止越界长度声明耗尽内存（32MiB）。
	MaxPayload = 32 << 20
	// MFS/1 外层长度前缀（uint32 小端）。
	PrefixLen = 4
)

// 图像编码标识（线格式单字节）。
const (
	CodecPNG byte = 1
	CodecJPEG byte = 2
)

// 图像帧编码名称，用于清单与日志。
func CodecName(c byte) string {
	switch c {
	case CodecPNG:
		return "png"
	case CodecJPEG:
		return "jpeg"
	default:
		return fmt.Sprintf("unknown(%d)", c)
	}
}

// Frame 是两种容器格式解复用后的统一帧模型。
// 所有时间字段均为统一纳秒基准下的整数。
type Frame struct {
	CameraID uint16 // 源机位
	Sequence uint64 // 机位内序号
	EventNS  TimeNS // 事件时间（纳秒，已按机位时钟偏移校正）
	Version  byte   // 线格式版本
	Codec    byte   // 图像编码
	Width    uint32 // 图像宽度（像素）
	Height   uint32 // 图像高度（像素）
	Payload  []byte // 图像载荷字节（转封装全程原样保留）
	SHA256   [32]byte
	CRC32C   uint32

	// 解复用阶段记录的来源信息，不参与线格式序列化。
	SourceOffset int64  // 记录在输入文件中的字节偏移（长度前缀起点）
	SourceName   string // 输入文件名或 tar 成员名
}

// RecordLen 返回完整线格式记录长度（头 + 载荷 + 尾）。
func (f *Frame) RecordLen() int {
	return HeaderLen + len(f.Payload) + TailLen
}

// Clone 返回载荷独立的深拷贝，避免缓冲复用造成别名写入。
func (f *Frame) Clone() *Frame {
	cp := *f
	if f.Payload != nil {
		cp.Payload = append([]byte(nil), f.Payload...)
	}
	return &cp
}
