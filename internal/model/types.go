// Package model 定义播出流水线各子系统共享的样本、时基与清单类型。
// 这些类型是 probe / clock / fault / timeline / rendition / render / verify
// 之间的通用契约，任何子系统都不得引入反向依赖。
package model

// TrackKind 区分画面轨道与音频编码轨道。
type TrackKind uint8

const (
	KindVideo TrackKind = iota + 1
	KindOpus
	KindAAC
)

func (k TrackKind) String() string {
	switch k {
	case KindVideo:
		return "video"
	case KindOpus:
		return "opus"
	case KindAAC:
		return "aac"
	default:
		return "unknown"
	}
}

// Packet 是探针读出的原始可变时长包。PTS/Duration 均为轨道本地时基下的
// 整数滴答，永远保留供应商写入的原始值；归一化只发生在 clock/timeline 层。
type Packet struct {
	TrackID   uint8
	Kind      TrackKind
	Seq       uint64 // 轨道内单调序号（丢包会断号）
	PTS       uint64 // 原始 PTS，可能已回绕
	Duration  uint32 // 原始包时长（本地滴答），VFR 轨道逐包变化
	Payload   []byte
	ByteStart int64  // 在源素材中的起始字节偏移（截断报告用）
	ByteEnd   int64  // 包结束字节偏移（不含）
	Checksum  uint32 // 生成端写入的 CRC-32
}

// Sample 是统一到主时基后的不可变样本。
type Sample struct {
	TrackID  uint8
	Kind     TrackKind
	Seq      uint64
	PTS      int64 // 归一化起始时间（主时基滴答）
	Duration int64 // 归一化时长（主时基滴答）
	Wrap     int   // 该样本经历的回绕次数
	Dupe     bool  // 是否为重复包（保留但标记）
	Payload  []byte
}

// Gap 记录丢包造成的序号缺口，时间线上保留为空洞而非插值填补。
type Gap struct {
	TrackID   uint8
	Kind      TrackKind
	Missing   []uint64 // 缺失序号
	AfterSeq  uint64   // 缺口前最后一个收到的序号
	BeforeSeq uint64   // 缺口后第一个收到的序号
	StartPTS  int64    // 归一化空洞起点
	EndPTS    int64    // 归一化空洞终点
}

// Fault 是非致命异常的结构化记录（损坏、截断、丢包等仍可继续处理）。
type Fault struct {
	Kind      string // fault.Kind* 的字符串形式，避免 model→fault 反向依赖
	TrackID   uint8
	Seq       uint64
	ByteStart int64
	ByteEnd   int64
	Detail    string
}

// Plan 描述一次素材生成的全部确定性参数，同时充当读取侧的清单。
type Plan struct {
	Seed        int64
	DurationMS  int64 // 素材总时长（毫秒）
	Tracks      []TrackPlan
	Faults      FaultPlan
	SegmentMS   int64 // 检查点/恢复段长度
	WrapModulus map[uint8]uint64
}

// TrackPlan 描述单条轨道的编码与节奏。
type TrackPlan struct {
	ID       uint8
	Kind     TrackKind
	Timebase uint64 // 本地时基滴答/秒
	PeriodUS int64  // 标称包周期（微秒）
	JitterUS int64  // VFR 抖动幅度（微秒），0 表示恒定帧率
	DriftPPM int64  // 时钟漂移（百万分率）
}

// FaultPlan 控制夹具注入的故障集合与数量。
type FaultPlan struct {
	EnableDrops   bool
	EnableDupes   bool
	EnableCorrupt bool
	EnableTrunc   bool
	DropCount     int
	DupeCount     int
	CorruptCount  int
	TruncCount    int
}

// FrameRef 指向导出的一帧 PNG 及其像素哈希。
type FrameRef struct {
	Index  int
	TimeUS int64
	Path   string
	Sha256 string
}
