package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"

	"medpipe/internal/clock"
	"medpipe/internal/ingest"
)

// runOptionsFile 是 demo 写出、resume 读取的运行参数文件名。
const runOptionsFile = "run.json"

// persistedOptions 是 Options 的可序列化形态（Fault 转为字符串）。
type persistedOptions struct {
	Seed         int64  `json:"seed"`
	Tracks       int    `json:"tracks"`
	Seconds      int    `json:"seconds"`
	FPSNum       int64  `json:"fps_num"`
	FPSDen       int64  `json:"fps_den"`
	TargetBPS    int64  `json:"target_bps"`
	Fault        string `json:"fault"`
	WindowSize   int64  `json:"window_size"`
	PacketFrames int64  `json:"packet_frames"`
	MaxBlock     int64  `json:"max_block"`
}

// SaveOptions 把运行参数写入输出目录，供 resume 复原。
func SaveOptions(dir string, opts Options) error {
	p := persistedOptions{
		Seed: opts.Seed, Tracks: opts.Tracks, Seconds: opts.Seconds,
		FPSNum: opts.FPSNum, FPSDen: opts.FPSDen, TargetBPS: opts.TargetBPS,
		Fault: string(opts.Fault), WindowSize: opts.WindowSize,
		PacketFrames: opts.PacketFrames, MaxBlock: opts.MaxBlock,
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return clock.WrapError(clock.CodeCheckpoint, -1, err, "运行参数序列化失败")
	}
	if err := os.WriteFile(filepath.Join(dir, runOptionsFile), data, 0o644); err != nil {
		return clock.WrapError(clock.CodeCheckpoint, -1, err, "运行参数写入失败")
	}
	return nil
}

// LoadOptions 从输出目录恢复运行参数。
func LoadOptions(dir string) (Options, error) {
	raw, err := os.ReadFile(filepath.Join(dir, runOptionsFile))
	if err != nil {
		return Options{}, clock.WrapError(clock.CodeCheckpoint, -1, err, "运行参数读取失败")
	}
	var p persistedOptions
	if err := json.Unmarshal(raw, &p); err != nil {
		return Options{}, clock.WrapError(clock.CodeCheckpoint, -1, err, "运行参数解析失败")
	}
	return Options{
		Seed: p.Seed, Tracks: p.Tracks, Seconds: p.Seconds,
		FPSNum: p.FPSNum, FPSDen: p.FPSDen, TargetBPS: p.TargetBPS,
		Fault: ingest.Fault(p.Fault), WindowSize: p.WindowSize,
		PacketFrames: p.PacketFrames, MaxBlock: p.MaxBlock,
	}, nil
}
