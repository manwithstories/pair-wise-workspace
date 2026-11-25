package model

import "fmt"

// 故障大类。各子系统统一登记到 Fault，位置由 Segment/Offset/Sample/Record 表达。
const (
	FaultManifestMissing  = "manifest_missing_segment"   // 清单序号缺失或引用文件不存在
	FaultManifestDuplicate = "manifest_duplicate_segment" // 清单内重复序号/引用
	FaultManifestMismatch = "manifest_hls_dash_mismatch"  // HLS 与 DASH 互查不一致
	FaultBoxTruncated     = "mp4_box_truncated"           // 盒长度越界/截断
	FaultTimeScaleZero    = "mp4_zero_timescale"          // mdhd/tfhd 时间尺度为零
	FaultSampleOverflow   = "mp4_sample_out_of_bounds"    // trun 样本超出 mdat
	FaultRTPReorder       = "rtp_reordered"               // 乱序
	FaultRTPWrap          = "rtp_sequence_wrap"           // 16 位序号回绕
	FaultRTPLoss          = "rtp_packet_loss"             // 丢包
	FaultRTPTimestampJump = "rtp_timestamp_jump"          // 时间戳跳变
	FaultOversized        = "fragment_oversized"          // 超长分片（超过内存边界）
	FaultCheckpointBad    = "checkpoint_corrupt"          // 检查点/索引校验失败
)

// Fault 是一条可定位的诊断记录。位置字段按需填写，未知为零值。
type Fault struct {
	Kind    string `json:"kind"`
	Segment int    `json:"segment"`              // 分片序号（媒体序号，从 0 起）
	Offset  int64  `json:"offset,omitempty"`     // 字节偏移（文件内）
	Sample  int    `json:"sample,omitempty"`     // 样本索引（分片内，从 0 起）
	Record  int    `json:"record,omitempty"`     // RTP 记录索引（文件内，从 0 起）
	Detail  string `json:"detail,omitempty"`
}

func (f Fault) String() string {
	switch {
	case f.Record > 0 || f.Kind[:3] == "rtp":
		return fmt.Sprintf("%s seg=%d record=%d %s", f.Kind, f.Segment, f.Record, f.Detail)
	case f.Sample > 0:
		return fmt.Sprintf("%s seg=%d off=%d sample=%d %s", f.Kind, f.Segment, f.Offset, f.Sample, f.Detail)
	default:
		return fmt.Sprintf("%s seg=%d off=%d %s", f.Kind, f.Segment, f.Offset, f.Detail)
	}
}

// SegmentRef 是清单中的一条分片引用。
type SegmentRef struct {
	Seq     int    // 媒体序号
	URI     string // 清单内相对引用
	Start   Rat    // 起始时间（纳秒有理数）
	Duration Rat   // 时长（纳秒有理数）
}

// SegmentMeta 是一个分片经过清单互查后的共享元数据。
type SegmentMeta struct {
	Seq      int
	File     string // 分片文件名（输入目录相对路径）
	Start    Rat
	Duration Rat
	Exists   bool
}

// SampleInfo 是 fMP4 分片内单个样本的解析结果。
type SampleInfo struct {
	Index   int
	IsSync  bool // 关键帧（trun/tfhd sample flags 的 sample_depends_on==2）
	Pts     Rat
	Size    int64
}

// FragmentReport 是一个分片的流式解析结论。
type FragmentReport struct {
	Seq          int
	TimeScale    uint32
	Samples      []SampleInfo
	SyncTimes    []Rat // 关键帧时间，用于计算间隔
	Faults       []Fault
	FirstBadOff  int64 // 首个盒结构错误的字节偏移，-1 表示无
	BytesInMDAT  int64
}

// RTPReport 是一个 RTP 诊断记录文件的解析结论。
type RTPReport struct {
	Packets  int
	Faults   []Fault
	MinSeq   uint16
	MaxExtSeq uint64 // 扩展后的 64 位序号（含回绕次数）
}

// ManifestReport 汇总清单解析与互查结论。
type ManifestReport struct {
	Segments []SegmentMeta
	Faults   []Fault
}
