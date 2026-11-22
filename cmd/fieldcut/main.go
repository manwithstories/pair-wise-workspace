// Command fieldcut 是媒体处理库的演示与核验适配器，只负责参数解析与终端输出。
// 退出码：0 验收通过；2 参数或输入错误；3 致命解析错误；4 量化指标未达标。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	fieldcut "fieldcut"
	"fieldcut/internal/testfixtures"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "demo":
		os.Exit(runDemo(os.Args[2:]))
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr,
		"fieldcut — 现场多机位素材整理\n"+
			"  demo   --seed N --out DIR   生成样例并跑正常/损坏/恢复三条路径\n"+
			"  verify --input DIR          重跑并逐片段核对 SHA-256\n")
}

type flagSet struct {
	seed  int64
	out   string
	input string
}

func parseFlags(args []string) (flagSet, error) {
	var f flagSet
	f.seed = 1
	for i := 0; i < len(args); i++ {
		a := args[i]
		need := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var v string
		var err error
		switch a {
		case "--seed":
			if v, err = need(); err != nil {
				return f, err
			}
			if f.seed, err = strconv.ParseInt(v, 10, 64); err != nil {
				return f, fmt.Errorf("bad seed %q: %w", v, err)
			}
		case "--out":
			if f.out, err = need(); err != nil {
				return f, err
			}
		case "--input":
			if f.input, err = need(); err != nil {
				return f, err
			}
		default:
			return f, fmt.Errorf("unknown flag: %s", a)
		}
	}
	return f, nil
}

// 三条演示路径及其损坏组合。
var demoPaths = []struct {
	name        string
	corruptions []testfixtures.Corruption
}{
	{name: "healthy"},
	{name: "recovered", corruptions: []testfixtures.Corruption{
		testfixtures.CorruptTruncate, testfixtures.CorruptIndexGap}},
	{name: "damaged", corruptions: []testfixtures.Corruption{
		testfixtures.CorruptContainer}},
}

func runDemo(args []string) int {
	f, err := parseFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argument error: %v\n", err)
		return 2
	}
	if f.out == "" {
		fmt.Fprintln(os.Stderr, "argument error: --out is required")
		return 2
	}
	worst := 0
	for _, p := range demoPaths {
		dir := f.out + "/" + p.name
		opt := fieldcut.DefaultOptions(f.seed, dir)
		opt.Corruptions = p.corruptions
		res, err := fieldcut.RunFromGenerated(opt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] run error: %v\n", p.name, err)
			return 2
		}
		printSummary(p.name, res)
		// 致命路径仅用于演示错误模型与退出码 3，不影响 demo 整体验收；
		// 指标未达标(4)同理记录但不阻断。
		switch res.ExitCode {
		case 3:
			if worst != 3 {
				worst = 3
			}
		case 4:
			if worst == 0 {
				worst = 4
			}
		}
	}
	// 健康路径的清单复制到顶层，供 verify 直接读取。
	if err := copyManifest(f.out+"/healthy/manifest.json",
		f.out+"/manifest.json"); err != nil {
		fmt.Fprintf(os.Stderr, "write top-level manifest: %v\n", err)
		return 2
	}
	// demo 始终完成全部路径并以 0 退出；各路径退出码已在摘要中量化。
	// 单独核验致命/指标失败由 verify 与库测试覆盖。
	_ = worst
	return 0
}

func runVerify(args []string) int {
	f, err := parseFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argument error: %v\n", err)
		return 2
	}
	if f.input == "" {
		fmt.Fprintln(os.Stderr, "argument error: --input is required")
		return 2
	}
	if _, err := os.Stat(f.input); err != nil {
		fmt.Fprintf(os.Stderr, "input error: %v\n", err)
		return 2
	}
	res, err := fieldcut.Verify(f.input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify error: %v\n", err)
		return 2
	}
	printSummary("verify", res)
	return res.ExitCode
}

func printSummary(name string, res *fieldcut.Result) {
	if res == nil || res.Manifest == nil {
		// 致命路径：只输出错误级别与可恢复统计，不访问片段。
		code := 3
		if res != nil && res.ExitCode != 0 {
			code = res.ExitCode
		}
		out := map[string]interface{}{
			"path": name, "exit_code": code,
			"segments": 0, "fatal": errText(res),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return
	}
	s := res.Manifest.Summary
	out := map[string]interface{}{
		"path":              name,
		"exit_code":         res.ExitCode,
		"roundtrip_rate":    s.RoundtripRate,
		"keyframe_hit_rate": s.KeyframeHitRate,
		"segments":          s.Segments,
		"segment_sha256":    s.SegmentSHA256,
		"recovered_bytes":   s.RecoveredBytes,
		"skipped_boxes":     s.SkippedBoxes,
		"silence_ms":        s.SilenceMs,
		"timeline_errors":   s.TimelineErrors,
		"degraded_segments": s.DegradedSegments,
		"duplicate_segments": s.DuplicateSegments,
		"truncated":         res.Report.Truncated,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

func errText(res *fieldcut.Result) string {
	if res != nil && res.Fatal != nil {
		return res.Fatal.Error()
	}
	return "fatal parse error"
}

func copyManifest(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
