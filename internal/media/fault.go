package media

import (
	"errors"
	"fmt"
)

// 解析/处理阶段标识，随错误一起上报，定位首个错误发生的位置。
const (
	StagePrefix    = "prefix"    // 外层长度前缀
	StageHeader    = "header"    // 记录头
	StageLength    = "length"    // 长度越界 / 记录不完整
	StageVersion   = "version"   // 未知版本
	StagePayload   = "payload"   // 载荷读取
	StageSHA       = "sha256"    // 载荷哈希不符
	StageCRC       = "crc32c"    // CRC32C 不符
	StageTar       = "tar"       // TAR 归档结构
	StageCheckpoint = "checkpoint"
	StageConfig    = "config"
	StageQuarantine = "quarantine"
)

// 退出码：与验收协议固定对应。
const (
	ExitOK              = 0 // 完成
	ExitMediaCorruption = 2 // 已定位的媒体损坏
	ExitUsage           = 3 // 参数错误
	ExitCheckpoint      = 4 // 检查点/输入/映射校验不符
	ExitInterrupt       = 5 // 可恢复的模拟中断
)

// Fault 是全系统统一错误模型，携带定位首个错误所需的全部坐标。
type Fault struct {
	Stage      string // 发生阶段
	ByteOffset int64  // 文件字节偏移（长度前缀起点）
	CameraID   uint16 // 出错记录机位（无法解析时为 0）
	Sequence   uint64 // 出错记录序号（无法解析时为 0）
	OffsetErr  int    // 0=偏移精确；1=偏移为估算
	Err        error  // 包裹的底层错误
}

func (e *Fault) Error() string {
	return fmt.Sprintf("media fault stage=%s offset=%d camera=%d seq=%d: %v",
		e.Stage, e.ByteOffset, e.CameraID, e.Sequence, e.Err)
}

func (e *Fault) Unwrap() error { return e.Err }

// NewFault 构造一个偏移精确的错误。
func NewFault(stage string, offset int64, cam uint16, seq uint64, format string, args ...any) *Fault {
	return &Fault{
		Stage:      stage,
		ByteOffset: offset,
		CameraID:   cam,
		Sequence:   seq,
		OffsetErr:  0,
		Err:        fmt.Errorf(format, args...),
	}
}

// MarkEstimated 将偏移标记为估算值（例如截断尾部无法定位记录起点）。
func (e *Fault) MarkEstimated() *Fault { e.OffsetErr = 1; return e }

// AsFault 从错误链中取出 *Fault；不存在则返回 nil。
func AsFault(err error) *Fault {
	var f *Fault
	if errors.As(err, &f) {
		return f
	}
	return nil
}
