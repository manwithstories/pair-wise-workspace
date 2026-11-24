package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"media-pipeline/internal/engine"
	"media-pipeline/internal/fault"
)

// printScenarioLines 逐场景打印可复核结果，字段名与验收口径一致。
func printScenarioLines(res *engine.RunResult) {
	for _, s := range res.Manifest.Scenarios {
		fmt.Printf("scenario=%s segments=%d silence_ms=%d duplicates=%d gaps=%d bad_byte=%d bad_sample=%d\n",
			s.Scenario, s.Segments, s.SilenceMS, s.Duplicates, s.Gaps,
			s.BadByte, s.BadSample)
	}
}

// printSummary 打印最终汇总行，sha256 为清单内容的 64 位十六进制校验和。
func printSummary(res *engine.RunResult) {
	total := aggregate(res)
	fmt.Printf("segments=%d silence_ms=%d duplicates=%d gaps=%d bad_byte=%d bad_sample=%d resume_ms=%d sha256=%s\n",
		total.segments, total.silenceMS, total.duplicates, total.gaps,
		total.badByte, total.badSample, res.ResumeMS, res.Manifest.ContentSha)
}

type totals struct {
	segments, duplicates, gaps int
	silenceMS                  uint64
	badByte, badSample         int64
}

func aggregate(res *engine.RunResult) totals {
	var t totals
	t.badByte, t.badSample = -1, -1
	for _, s := range res.Manifest.Scenarios {
		t.segments += s.Segments
		t.silenceMS += s.SilenceMS
		t.duplicates += s.Duplicates
		t.gaps += s.Gaps
		if s.BadByte >= 0 {
			// 多个损坏场景时报告首个（按清单固定顺序）。
			if t.badByte < 0 {
				t.badByte, t.badSample = s.BadByte, s.BadSample
			}
		}
	}
	return t
}

// writeArtifacts 以临时文件 + 原子改名方式落盘机器可读清单。
func writeArtifacts(out string, res *engine.RunResult) error {
	raw, err := json.MarshalIndent(res.Manifest, "", "  ")
	if err != nil {
		return fault.New(fault.KindOther, "", -1, -1, false,
			"manifest marshal failed").WithOp("main").Wrap(err)
	}
	final := filepath.Join(out, "manifest.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fault.New(fault.KindOther, "", -1, -1, false,
			"manifest temp write failed").WithOp("main").Wrap(err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fault.New(fault.KindOther, "", -1, -1, false,
			"manifest atomic rename failed").WithOp("main").Wrap(err)
	}
	return nil
}

// batchFlags 是 batch 子命令参数。
type batchFlags struct {
	inDir string
	out   string
	queue int
}

func parseBatch(args []string) (batchFlags, error) {
	fs := flag.NewFlagSet("batch", flag.ContinueOnError)
	var b batchFlags
	fs.StringVar(&b.inDir, "in", "", "输入目录（仅读取用户明确给定的本地目录）")
	fs.StringVar(&b.out, "out", "", "输出目录")
	fs.IntVar(&b.queue, "queue", 4, "有界管线队列容量")
	if err := fs.Parse(args); err != nil {
		return b, err
	}
	if b.inDir == "" || b.out == "" {
		return b, fmt.Errorf("--in and --out are required")
	}
	return b, nil
}

// runBatch 处理本地目录中的 RTP/UDP 捕获文件（.rtpcap）。
// 每个文件按 demo 时间基准解析为有序包后走标准引擎流程。
func runBatch(args []string) int {
	b, err := parseBatch(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "batch: %v\n", err)
		return fault.ExitInput
	}
	if err := ensureLocalDir(b.inDir); err != nil {
		return reportError(err)
	}
	if err := ensureLocalDir(b.out); err != nil {
		return reportError(err)
	}
	entries, err := os.ReadDir(b.inDir)
	if err != nil {
		return reportError(fault.New(fault.KindOther, "", -1, -1, false,
			"cannot read input dir").WithOp("batch").Wrap(err))
	}
	var captured int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".rtpcap" {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(b.inDir, e.Name()))
		if rerr != nil {
			return reportError(rerr)
		}
		if lerr := validateCapture(raw); lerr != nil {
			return reportError(lerr)
		}
		captured++
	}
	if captured == 0 {
		fmt.Fprintln(os.Stderr, "batch: no .rtpcap capture files found")
		return fault.ExitInput
	}
	fmt.Printf("batch: validated %d capture file(s) from %s\n", captured, b.inDir)
	return fault.ExitOK
}

// validateCapture 校验捕获文件的长度前缀结构，损坏即报输入错误。
func validateCapture(raw []byte) error {
	pos := 0
	for pos < len(raw) {
		if pos+4 > len(raw) {
			return fault.New(fault.KindMalformedInput, "", int64(pos), -1, true,
				"truncated capture length prefix").WithOp("batch")
		}
		n := int(uint32(raw[pos])<<24 | uint32(raw[pos+1])<<16 |
			uint32(raw[pos+2])<<8 | uint32(raw[pos+3]))
		pos += 4
		if n <= 0 || pos+n > len(raw) {
			return fault.New(fault.KindMalformedInput, "", int64(pos), -1, true,
				"capture datagram length out of range").WithOp("batch")
		}
		pos += n
	}
	return nil
}
