// Package app 是命令适配器与流水线之间的编排层：解析终端参数、
// 串联 probe→clock/fault→timeline→rendition→resume/render→verify，
// 并把参数/输入错误与边界/校验错误区分开，供 main 映射退出码。
package app

import (
	"strings"
	"time"

	"bcast/internal/fault"
)

// Options 是 demo 子命令的全部终端参数。
type Options struct {
	WorkDir      string
	Seed         int64
	Duration     time.Duration
	FaultsAll    bool
	ResumeCutPct int // 0..100，模拟中断点占总时长百分比
	ReviewStep   time.Duration
}

// MaxFrames 固定导出帧上限，配合 30m/15s 恰为 120 帧。
const MaxFrames = 120

// SegmentMS 是封装/恢复段网格：60 秒一段。
const SegmentMS int64 = 60_000

// ParseDuration 接受 Go duration（30m、15s），非法输入返回 ErrInvalidInput。
func ParseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fault.InvalidInputf("invalid duration %q: %v", s, err)
	}
	return d, nil
}

// ParsePercent 解析 "61%" 形式的百分比，限定在 (0,100)。
func ParsePercent(s string) (int, error) {
	s = strings.TrimSpace(s)
	if !strings.HasSuffix(s, "%") {
		return 0, fault.InvalidInputf("percentage must end with %%: %q", s)
	}
	n := 0
	num := strings.TrimSuffix(s, "%")
	for _, c := range num {
		if c < '0' || c > '9' {
			return 0, fault.InvalidInputf("percentage has non-digit %q", num)
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 || n >= 100 {
		return 0, fault.InvalidInputf("percentage must be within (0,100): %d", n)
	}
	return n, nil
}

// ParseFaults 解析 --faults：all 注入四类故障，none 生成干净素材。
func ParseFaults(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "all":
		return true, nil
	case "none", "":
		return false, nil
	default:
		return false, fault.InvalidInputf("faults must be all or none, got %q", s)
	}
}

// Validate 做跨字段一致性校验。
func (o Options) Validate() error {
	if strings.TrimSpace(o.WorkDir) == "" {
		return fault.InvalidInputf("--work is required")
	}
	if o.Duration <= 0 {
		return fault.InvalidInputf("--duration must be positive")
	}
	if o.ReviewStep <= 0 {
		return fault.InvalidInputf("--review-step must be positive")
	}
	if o.ResumeCutPct <= 0 || o.ResumeCutPct >= 100 {
		return fault.InvalidInputf("--resume-cut must be within (0,100)")
	}
	if o.ReviewStep > o.Duration {
		return fault.InvalidInputf("review step %s exceeds duration %s", o.ReviewStep, o.Duration)
	}
	return nil
}
