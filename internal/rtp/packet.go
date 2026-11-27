// Package rtp 解析四字节大端长度前缀的 RTP/RTCP 数据报，
// 并提供有界重排序。所有解析错误都转换为带序号与字节偏移的 media.Fault。
package rtp

// 数据报类型（长度前缀之后首字节的区分位）。
const (
	// RTP 版本固定为 2；RTP 首字节高两位为 0b10，RTCP 为 0b10 且
	// payload type 落在 64..95（SR=200）。用 PT 区间区分两类报文。
	rtpVersion     = 2
	rtpHeaderMin   = 12
	rtpCSRCOffset  = 0x0F
	rtpExtBit      = 0x10
	rtpPaddingBit  = 0x20
	ptRTCPSender   = 200
	ptMask         = 0x7F
	markerBit      = 0x80
	rtcpHeaderMin  = 8
	rtcpSRMin      = 28 // 头 8 + SSRC 4 + 20 字节 sender info
	ntpEpochOffset = 2208988800
)

// Kind 标识数据报承载的协议类别。
type Kind uint8

const (
	KindUnknown Kind = iota
	KindRTP
	KindRTCP
)

// Datagram 是一条已解析的长度前缀数据报。
type Datagram struct {
	Kind     Kind
	Offset   int64 // 在输入文件中的起始偏移（长度前缀处）。
	Index    int64 // 数据报次序（0 起）。
	RawLen   int   // 长度前缀声明的字节数。
	IsRTCP   bool
	// RTP 字段
	Version     uint8
	PayloadType uint8
	SSRC        uint32
	Seq         uint16
	Timestamp   uint32 // 32 位 RTP 时钟，回绕扩展由 clock 包负责。
	Marker      bool
	Payload     []byte // 指向 Reader 内部缓冲的切片，下次 Read 后失效。
	// RTCP Sender Report 字段（仅 IsRTCP && PT==200 时有效）
	SRNTPSeconds  uint32 // NTP 秒
	SRNTPFraction uint32 // NTP 小数部分
	SRRTP         uint32 // SR 携带的 RTP 时间戳
	SRPacketCount uint32
	SROctetCount  uint32
}

// ClockRate 按负载类型给出 RTP 时钟基率。
// 本工程只生成 L16 音频（动态 PT 96..127）与视频标记（动态号），
// 具体基率由流参数注入；此处仅返回调用方给定的静态音频基率。
func (d *Datagram) ClockRate(staticAudioRate int) int {
	if d.IsRTCP {
		return 0
	}
	return staticAudioRate
}
