// Package model 定义帧、分片与清单的共享数据结构。
// 所有时间字段统一使用整数纳秒，禁止使用浮点存储媒体时间。
package model

// FaultCode 标识帧级故障类别。
type FaultCode string

const (
	FaultNone         FaultCode = ""              // 无故障
	FaultHashMismatch FaultCode = "hash_mismatch" // SHA-256 校验不符
	FaultMetaMissing  FaultCode = "meta_missing"  // 元数据缺失
	FaultClockBack    FaultCode = "clock_back"    // 时间戳倒退
	FaultLost         FaultCode = "lost"          // 传输不可恢复丢帧
)

// Frame 是一帧图像及其拍摄/传输元数据。
type Frame struct {
	Rep          string // 表示层名称，如 360p / 720p
	Seq          uint32 // 帧序号（从 0 开始）
	IsKey        bool   // 是否关键帧
	RTPSeq       uint16 // RTP 包序号
	RTPTimestamp uint32 // 32 位 RTP 时间戳（90kHz 时钟）
	AudioSamples uint32 // 截至本帧的累计音频采样数（漂移锚点）
	TruthNS      int64  // 生成侧真实媒体时间（仅测试/评估使用，恢复侧禁止读取）
	PayloadHash  []byte // 有效载荷的 SHA-256
	Payload      []byte // 有效载荷字节
}

// CheckResult 记录单帧校验与恢复结果。
type CheckResult struct {
	Seq      uint32
	OK       bool
	Fault    FaultCode
	Repaired bool // 是否经冗余恢复成功
	MediaNS  int64 // 重建后的统一媒体时间（纳秒）
}

// Gap 描述无法恢复的帧区间（半开 [Start,End)）。
type Gap struct {
	Start uint32
	End   uint32
}

// Segment 是清单中的一个分片，全部字段由共享时钟核验后填写。
type Segment struct {
	Index         int
	Rep           string
	FirstSeq      uint32
	LastSeq       uint32
	StartNS       int64
	EndNS         int64
	PayloadSHA256 []byte // 分片内全部帧载荷拼接后的摘要
	TailIncomplete bool  // 尾分片未达到目标时长
	ToleranceNS   int64 // 边界相对目标时长的容差
}

// Manifest 是单个表示层的分片清单。
type Manifest struct {
	Rep       string
	Segments  []Segment
	Gaps      []Gap
	MediaSpan int64 // 覆盖的媒体时间跨度（纳秒）
}

// DigestInput 是参与输入摘要计算的确定性参数。
type DigestInput struct {
	Frames     int
	DurationNS int64
	Bitrates   string
	DriftPPM   int
	WrapAt     uint64
	LossEvery  int
	Corrupt    int
}
