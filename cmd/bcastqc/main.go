// Command bcastqc 是广播节目包 QC 管线的终端适配器，
// 提供 demo（生成样例并跑全流程）与 verify（独立回读核对）两个子命令。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bcastqc/internal/ebml"
	"bcastqc/internal/fixture"
	"bcastqc/internal/media"
	"bcastqc/internal/mux"
	"bcastqc/internal/pipeline"
)

// 退出码约定：0 全部预期成立；2 参数/输入错误；3 I/O、中断或意外不完整。
const (
	exitOK       = 0
	exitArgError = 2
	exitIOError  = 3
)

// 验收阈值（与需求一致）。
const (
	minRecoveryPct = 95.0
	maxSyncMS      = 20
	maxLatencyP95  = 250
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return exitArgError
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", args[0])
		printUsage()
		return exitArgError
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "用法:")
	fmt.Fprintln(os.Stderr, "  bcastqc demo --suite clean,recovery,truncate,overflow,stall \\")
	fmt.Fprintln(os.Stderr, "    --seed 42 --queue-bytes 4194304 --out-dir <dir>")
	fmt.Fprintln(os.Stderr, "  bcastqc verify --dir <dir>")
}

type demoFlags struct {
	suite      string
	seed       int64
	queueBytes int64
	outDir     string
}

func parseDemoFlags(args []string) (demoFlags, error) {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	var f demoFlags
	fs.StringVar(&f.suite, "suite", strings.Join(fixture.AllSuites, ","), "逗号分隔的场景套件")
	fs.Int64Var(&f.seed, "seed", 42, "确定性随机种子")
	fs.Int64Var(&f.queueBytes, "queue-bytes", 4<<20, "单文件队列字节上限")
	fs.StringVar(&f.outDir, "out-dir", "", "再封装输出目录")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if f.outDir == "" {
		return f, fmt.Errorf("--out-dir 为必填参数")
	}
	if f.queueBytes <= 0 {
		return f, fmt.Errorf("--queue-bytes 必须为正数")
	}
	return f, nil
}

func runDemo(args []string) int {
	f, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "参数错误: %v\n", err)
		return exitArgError
	}
	suites := parseSuites(f.suite)
	if len(suites) == 0 || len(suites) > 32 {
		fmt.Fprintf(os.Stderr, "参数错误: 套件数量需在 1~32，得到 %d\n", len(suites))
		return exitArgError
	}
	if err := os.MkdirAll(f.outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "I/O 错误: 无法创建输出目录: %v\n", err)
		return exitIOError
	}

	specs := make([]fixture.Spec, len(suites))
	for i, s := range suites {
		specs[i] = fixture.SpecFor(s, f.seed, f.queueBytes)
	}
	results, err := pipeline.Run(context.Background(), specs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "管线错误: %v\n", err)
		if media.IsKind(err, media.KindArg) {
			return exitArgError
		}
		return exitIOError
	}

	manifest := make([]pipeline.Metrics, 0, len(results))
	allExpectationsMet := true
	for _, res := range results {
		m := res.Metrics
		manifest = append(manifest, m)
		if m.Rejected {
			// 预期拒绝（如 truncate）：不写半成品，记为通过。
			if m.FirstMissingOff < 0 {
				// 拒绝样例必须报告首个缺失偏移，否则视为意外不完整。
				allExpectationsMet = false
			}
			continue
		}
		// 落盘再封装结果。
		outPath := filepath.Join(f.outDir, m.Case+".webm")
		if err := os.WriteFile(outPath, res.Output, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "I/O 错误: 写 %s 失败: %v\n", outPath, err)
			return exitIOError
		}
		if !acceptHealthy(m) {
			allExpectationsMet = false
		}
	}

	printTable(manifest)
	manifestPath := filepath.Join(f.outDir, "manifest.json")
	if err := writeManifest(manifestPath, manifest); err != nil {
		fmt.Fprintf(os.Stderr, "I/O 错误: %v\n", err)
		return exitIOError
	}
	if !allExpectationsMet {
		fmt.Fprintln(os.Stderr, "验收阈值未全部满足")
		return exitIOError
	}
	return exitOK
}

// acceptHealthy 对非拒绝样例按场景核对验收阈值。
// 250ms 时延与 95% 恢复率阈值针对 recovery 场景；
// overflow/stall 为压力/停顿场景，仅约束缓存字节硬上限与基础同步。
func acceptHealthy(m pipeline.Metrics) bool {
	if m.SyncMaxMS > maxSyncMS {
		return false
	}
	if m.PeakBufferBytes > 4<<20 {
		return false
	}
	switch m.Case {
	case fixture.SuiteRecovery:
		if m.RecoveryPct < minRecoveryPct {
			return false
		}
		if m.LatencyP95MS > maxLatencyP95 {
			return false
		}
	case fixture.SuiteClean:
		if m.RecoveryPct < minRecoveryPct || m.LatencyP95MS > maxLatencyP95 {
			return false
		}
	}
	return true
}

func parseSuites(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func printTable(manifest []pipeline.Metrics) {
	header := []string{
		"case", "sync_max_ms", "lost", "recovered", "recovery_pct",
		"latency_p95_ms", "peak_buffer_bytes", "output_sha256", "first_missing_offset",
	}
	fmt.Println(strings.Join(header, "\t"))
	for _, m := range manifest {
		fmt.Printf("%s\t%d\t%d\t%d\t%.2f\t%d\t%d\t%s\t%d\n",
			m.Case, m.SyncMaxMS, m.Lost, m.Recovered, m.RecoveryPct,
			m.LatencyP95MS, m.PeakBufferBytes, dashIfEmpty(m.OutputSHA256), m.FirstMissingOff)
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeManifest(path string, manifest []pipeline.Metrics) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	dir := fs.String("dir", "", "demo 输出目录")
	if err := fs.Parse(args); err != nil {
		return exitArgError
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "参数错误: --dir 为必填参数")
		return exitArgError
	}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "I/O 错误: 读取目录失败: %v\n", err)
		return exitIOError
	}
	checked := 0
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".webm" {
			continue
		}
		path := filepath.Join(*dir, ent.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "I/O 错误: %v\n", err)
			return exitIOError
		}
		doc, err := ebml.ParseDocument(ebml.NewBytesReader(raw))
		if err != nil {
			fmt.Fprintf(os.Stderr, "核对失败 %s: %v\n", ent.Name(), err)
			return exitIOError
		}
		blocks := make([]mux.MuxBlock, 0, len(doc.Blocks))
		for _, b := range doc.Blocks {
			blocks = append(blocks, mux.MuxBlock{
				Track: b.Track, TimeNS: b.TimeNS, Key: b.Keyframe, Payload: b.Payload,
			})
		}
		vr, err := mux.Verify(raw, blocks)
		if err != nil || !vr.PayloadOK || !vr.Monotonic {
			fmt.Fprintf(os.Stderr, "核对失败 %s: %+v err=%v\n", ent.Name(), vr, err)
			return exitIOError
		}
		sum := sha256.Sum256(raw)
		fmt.Printf("%s\tblocks=%d\ttracks=%d\tsha256=%s\tOK\n",
			ent.Name(), vr.BlockCount, vr.TrackCount, hex.EncodeToString(sum[:]))
		checked++
	}
	if checked == 0 {
		fmt.Fprintln(os.Stderr, "输入错误: 目录中没有可核对的 .webm 文件")
		return exitArgError
	}
	fmt.Printf("独立核对通过: %d 个文件\n", checked)
	return exitOK
}
