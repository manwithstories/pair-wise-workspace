// Package fault 定义跨 ingest、timeline、archive、render 传播的错误与审计模型。
package fault

import (
	"errors"
	"fmt"
)

// Exit 码与验收约定保持一致。
const (
	ExitOK            = 0
	ExitUsage         = 2
	ExitUnrecoverable = 3
	ExitIntegrity     = 4
	ExitCheckpoint    = 75
)

// Kind 区分参数、结构损坏、身份冲突、空洞、迟到和完整性错误。
type Kind string

const (
	KindUsage       Kind = "usage"
	KindStructure   Kind = "structure"
	KindBounds      Kind = "bounds"
	KindFormat      Kind = "format"
	KindConflict    Kind = "identity_conflict"
	KindHole        Kind = "sequence_hole"
	KindLate        Kind = "late_behind_watermark"
	KindCheckpoint  Kind = "checkpoint"
	KindIntegrity   Kind = "integrity"
	KindUnsupported Kind = "unsupported"
)

// Location 精确定位源文件中的 box、RTP 记录或样本偏移。
type Location struct {
	Source  string `json:"source"`
	Box     string `json:"box,omitempty"`
	Offset  int64  `json:"offset"`
	Length  int64  `json:"length,omitempty"`
	End     int64  `json:"end,omitempty"`
	Track   string `json:"track,omitempty"`
	Packet  int64  `json:"packet,omitempty"`
	Sample  int    `json:"sample,omitempty"`
	Message string `json:"message,omitempty"`
}

// Error 是项目内统一错误，可携带归档报告所需位置。
type Error struct {
	Kind        Kind      `json:"kind"`
	Message     string    `json:"message"`
	Location    *Location `json:"location,omitempty"`
	Recoverable bool      `json:"recoverable"`
	Quarantined bool      `json:"quarantined,omitempty"`
	Err         error     `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Location != nil && e.Location.Source != "" {
		return fmt.Sprintf("%s: %s at %s offset=%d", e.Kind, e.Message, e.Location.Source, e.Location.Offset)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// New 构造统一错误。
func New(kind Kind, msg string, loc *Location) *Error {
	recoverable := kind == KindConflict || kind == KindHole || kind == KindLate || kind == KindStructure
	return &Error{Kind: kind, Message: msg, Location: loc, Recoverable: recoverable}
}

// Wrap 包装底层错误。
func Wrap(kind Kind, msg string, loc *Location, err error) *Error {
	e := New(kind, msg, loc)
	e.Err = err
	return e
}

// AsError 尝试转换为 *Error。
func AsError(err error) (*Error, bool) {
	var fe *Error
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

// ExitCode 将错误映射到命令行退出码。
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var fe *Error
	if errors.As(err, &fe) {
		switch fe.Kind {
		case KindUsage:
			return ExitUsage
		case KindIntegrity:
			return ExitIntegrity
		case KindCheckpoint:
			return ExitCheckpoint
		case KindStructure, KindBounds, KindFormat, KindUnsupported:
			if fe.Recoverable {
				return ExitOK
			}
			return ExitUnrecoverable
		default:
			return ExitUnrecoverable
		}
	}
	return ExitUnrecoverable
}

// Finding 是写入归档报告的一条审计发现。
type Finding struct {
	Code      string    `json:"code"`
	Severity  string    `json:"severity"`
	Message   string    `json:"message"`
	Location  *Location `json:"location,omitempty"`
	Recovered bool      `json:"recovered"`
}

// Hole 表示由扩展序号缺口和事件时间共同确定的空洞区间。
type Hole struct {
	Track        string `json:"track"`
	FirstMissing uint64 `json:"first_missing_ext_seq"`
	LastMissing  uint64 `json:"last_missing_ext_seq"`
	StartTicks   int64  `json:"start_ticks"`
	EndTicks     int64  `json:"end_ticks"`
	StartSource  int64  `json:"start_source_offset,omitempty"`
	EndSource    int64  `json:"end_source_offset,omitempty"`
}

// Conflict 记录相同身份但内容哈希不同的包。
type Conflict struct {
	Track     string     `json:"track"`
	ExtSeq    uint64     `json:"ext_seq"`
	FirstHash string     `json:"first_hash"`
	OtherHash string     `json:"other_hash"`
	Locations []Location `json:"locations"`
}

// LateImpact 记录跨水位迟到包影响的回放和帧范围。
type LateImpact struct {
	Track            string    `json:"track"`
	ExtSeq           uint64    `json:"ext_seq"`
	EventEndTicks    int64     `json:"event_end_ticks"`
	WatermarkTicks   int64     `json:"watermark_ticks"`
	CheckpointCursor int       `json:"checkpoint_cursor"`
	ReplayedFrom     int       `json:"replayed_from"`
	ReplayedTo       int       `json:"replayed_to"`
	AffectedFrames   []int     `json:"affected_frames"`
	Location         *Location `json:"location,omitempty"`
}

// Report 是 archive/report.json 的内存模型。
type Report struct {
	Tracks    int          `json:"tracks"`
	Findings  []Finding    `json:"findings"`
	Holes     []Hole       `json:"holes"`
	Conflicts []Conflict   `json:"conflicts"`
	Late      []LateImpact `json:"late_events"`
}

// AddFinding 追加发现并保证切片非 nil 以便 JSON 稳定。
func (r *Report) AddFinding(f Finding) { r.Findings = append(r.Findings, f) }
