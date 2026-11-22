// Package fault 定义带输入位置和恢复策略的统一错误模型。
package fault

import "fmt"

// Kind 错误类别。
type Kind string

const (
	KindManifest   Kind = "manifest"   // 清单结构/时间线错误
	KindIndex      Kind = "index"      // 索引与摘要错误
	KindTransport  Kind = "transport"  // RTP 传输错误
	KindAudio      Kind = "audio"      // 音频转换错误
	KindLoudness   Kind = "loudness"   // 响度/峰值门限错误
	KindPackage    Kind = "package"    // 封装错误
	KindCorrupt    Kind = "corrupt"    // 输入数据损坏
	KindResume     Kind = "resume"     // 续跑状态不可信
)

// Recovery 恢复策略。
type Recovery string

const (
	Fail        Recovery = "fail"         // 立即失败
	FillSilence Recovery = "fill_silence" // 缺口填充静音，时间线不压缩
	Skip        Recovery = "skip"         // 跳过该单元继续
	Rescan      Recovery = "rescan"      // 丢弃状态并重新扫描
)

// Error 是可定位错误：携带文件、行/段位置与恢复策略。
type Error struct {
	Kind     Kind
	Recovery Recovery
	File     string // 输入文件（可为空）
	Line     int    // 行号（清单类错误；0 表示不适用）
	Segment  int    // 段序号（-1 表示不适用）
	Msg      string
}

func (e *Error) Error() string {
	loc := e.File
	if e.Line > 0 {
		loc += fmt.Sprintf(":%d", e.Line)
	}
	if e.Segment >= 0 {
		loc += fmt.Sprintf(" seg#%d", e.Segment)
	}
	if loc == "" {
		loc = "(no-location)"
	}
	return fmt.Sprintf("[%s/%s] %s: %s", e.Kind, e.Recovery, loc, e.Msg)
}

// New 构造可定位错误。segment 传 -1 表示不适用。
func New(kind Kind, rec Recovery, file string, line, segment int, format string, args ...any) *Error {
	return &Error{Kind: kind, Recovery: rec, File: file, Line: line, Segment: segment,
		Msg: fmt.Sprintf(format, args...)}
}

// As 辅助：判断 err 是否为 *Error 并取出。
func As(err error) (*Error, bool) {
	if e, ok := err.(*Error); ok {
		return e, true
	}
	return nil, false
}
