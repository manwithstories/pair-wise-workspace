// Command tsproof 是 MPEG-TS 区域直播分发核验的纯终端批处理入口。
//
// 用法：
//
//	tsproof demo --seed <uint64> --out <dir>
//
// 退出码：0 核验通过；1 核验不一致；2 参数或输入不可读。
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"tsproof/internal/demo"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "demo":
		return runDemo(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", args[0])
		usage()
		return 2
	}
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seedFlag := fs.String("seed", "", "固定随机种子（无符号整数）")
	outFlag := fs.String("out", "", "产物输出目录")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *seedFlag == "" || *outFlag == "" {
		fmt.Fprintln(os.Stderr, "demo requires --seed and --out")
		usage()
		return 2
	}
	seed, err := strconv.ParseUint(*seedFlag, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --seed %q: %v\n", *seedFlag, err)
		return 2
	}
	if err := os.MkdirAll(*outFlag, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "cannot open --out %q: %v\n", *outFlag, err)
		return 2
	}

	m, code := demo.Run(seed, *outFlag)
	printReport(m)
	return code
}

func printReport(m demo.Metrics) {
	fmt.Println("== tsproof verification report ==")
	fmt.Printf("seed=%d\n", m.Seed)
	fmt.Println("-- clean path --")
	fmt.Printf("roundtrip_consistency=%s\n", m.RoundtripConsistency)
	fmt.Printf("keyframe_hit=%s\n", m.KeyframeHit)
	fmt.Printf("segments=%d\n", m.Segments)
	fmt.Printf("hash_match=%s\n", m.HashMatch)
	fmt.Println("-- damage path --")
	fmt.Printf("cc_gaps=%d psi_switches=%d crc_reject=%d truncated=%d\n",
		m.CCGaps, m.PSISwitches, m.CRCReject, m.Truncated)
	fmt.Printf("duplicates=%d reorders=%d pcr_wraps=%d\n",
		m.Duplicates, m.Reorders, m.PCRWraps)
	fmt.Println("-- recovery path --")
	fmt.Printf("resume_segments=%d sha256_match=%s\n", m.ResumeSegments, m.HashMatch)
	fmt.Printf("replay_identical=%s\n", m.ReplayIdentical)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tsproof demo --seed <uint64> --out <dir>")
}
