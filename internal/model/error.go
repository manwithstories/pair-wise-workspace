package model

import "errors"

// 跨模块共享的哨兵错误，各模块统一使用 errors.Is 判别。
var (
	// ErrInvalidInput 输入文件、参数或记录不合法（退出码 2）。
	ErrInvalidInput = errors.New("invalid input")
	// ErrUnsupportedCodec 遇到不支持的编码格式（仅支持 PCM）。
	ErrUnsupportedCodec = errors.New("unsupported codec")
	// ErrRiffStructure RIFF/WAVE 容器结构损坏或长度越界。
	ErrRiffStructure = errors.New("riff structure error")
	// ErrPacketConflict 同一轨道同一序号出现内容不同的包。
	ErrPacketConflict = errors.New("packet conflict")
	// ErrCRC 包或数据块 CRC 校验失败。
	ErrCRC = errors.New("crc mismatch")
	// ErrGraphCycle 轨道依赖图存在环路。
	ErrGraphCycle = errors.New("dependency cycle")
	// ErrClockDrift 跨轨道采样时钟漂移超出阈值。
	ErrClockDrift = errors.New("clock drift")
	// ErrOutOfRange 采样点超出轨道声明的合法范围。
	ErrOutOfRange = errors.New("sample out of range")
	// ErrCheckpoint 检查点读写或原子替换失败。
	ErrCheckpoint = errors.New("checkpoint error")
	// ErrUnrecoverable 恢复后仍无法通过校验（退出码 3）。
	ErrUnrecoverable = errors.New("unrecoverable")
	// ErrInterrupted 流水线在指定步骤后被中断，用于断点续跑演示。
	ErrInterrupted = errors.New("pipeline interrupted")
)

// ExitCode 将流水线错误映射为终端退出码：
// 0 成功；2 输入/参数无效；3 无法恢复或校验失败。
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, ErrInvalidInput),
		errors.Is(err, ErrUnsupportedCodec),
		errors.Is(err, ErrRiffStructure):
		return 2
	case errors.Is(err, ErrInterrupted):
		// 受控中断由演示驱动捕获并续跑，不直接决定最终退出码。
		return 0
	default:
		return 3
	}
}
