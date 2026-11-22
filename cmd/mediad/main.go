// mediad 是本地媒体流水线的命令行适配器，只依赖终端参数与输出。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mediad/internal/model"
	"mediad/internal/pipeline"
)

const usage = `mediad — 本地多轨媒体处理流水线

用法:
  mediad demo [--seed N] [--tracks N] [--resume] [--workdir DIR] [--tamper-cache]

退出码:
  0 全部断言通过
  1 核验失败（指标未达下限或哈希不一致）
  2 参数或输入无效
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	case "demo":
		return runDemo(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n%s", args[0], usage)
		return 2
	}
}

func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 41, "随机种子，保证夹具确定性")
	tracks := fs.Int("tracks", 3, "生成轨道数（1..4）")
	resume := fs.Bool("resume", false, "启用检查点恢复重跑")
	workdir := fs.String("workdir", "", "工作目录（默认 ./.mediad-work）")
	tamper := fs.Bool("tamper-cache", true, "注入缓存篡改以演示核对拒绝")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tracks < 1 || *tracks > 4 {
		fmt.Fprintf(os.Stderr, "无效 --tracks=%d：允许范围 1..4\n", *tracks)
		return 2
	}
	dir := *workdir
	if dir == "" {
		dir = filepath.Join(".", ".mediad-work")
	}
	if !safePath(dir) {
		fmt.Fprintf(os.Stderr, "拒绝路径: %s\n", dir)
		return 2
	}

	cfg := pipeline.Config{
		WorkDir:     dir,
		Seed:        *seed,
		Tracks:      *tracks,
		Resume:      *resume,
		TamperCache: *tamper,
	}
	runner, err := pipeline.NewRunner(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化失败: %v\n", err)
		return 2
	}

	// 第一次运行：建立片段、缓存与检查点。
	first, m1, err := runner.Run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "流水线运行失败: %v\n", err)
		return 2
	}
	fmt.Printf("first   %s\n", m1.Line())

	// 恢复运行：同输入重跑，检查点去重，片段计数不得增加。
	second, m2, err := runner.Run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "恢复运行失败: %v\n", err)
		return 2
	}
	fmt.Printf("resume  %s\n", m2.Line())

	return assertMetrics(cfg, first, second, m1, m2)
}

// assertMetrics 执行验收断言，返回对应退出码。
func assertMetrics(cfg pipeline.Config, first, second *model.Manifest,
	m1, m2 *pipeline.Metrics) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "FAIL "+format+"\n", a...)
		return 1
	}
	if m1.RoundtripTotal == 0 || m1.RoundtripOK != m1.RoundtripTotal {
		return fail("正常片段往返一致率须为 100%%: %s", m1.Line())
	}
	if m1.HashTotal == 0 || m1.HashOK != m1.HashTotal {
		return fail("逐片段哈希复核率须为 100%%: %s", m1.Line())
	}
	if m1.KeyframeTotal == 0 || m1.KeyframeHits*100 < m1.KeyframeTotal*90 {
		return fail("关键帧索引命中率须 >= 90%%: %s", m1.Line())
	}
	// 仅在注入缓存篡改时才必须产生拒绝记录。
	if cfg.TamperCache && m1.CacheRejected == 0 {
		return fail("缓存篡改必须被拒绝: %s", m1.Line())
	}
	// 恢复运行不得重复计数。
	if m2.HashTotal != m1.HashTotal || m2.RoundtripTotal != m1.RoundtripTotal {
		return fail("恢复运行出现重复计数: first=%s resume=%s", m1.Line(), m2.Line())
	}
	// 恢复后哈希须与干净运行逐片段一致。
	if !digestsEqual(first, second) {
		return fail("恢复后片段哈希与干净运行不一致")
	}
	fmt.Println("PASS 全部断言通过：往返一致率 100%，哈希复核 100%，恢复哈希与干净运行一致")
	return 0
}

// digestsEqual 逐片段比较两次运行的内容指纹。
func digestsEqual(a, b *model.Manifest) bool {
	if len(a.Segments) != len(b.Segments) {
		return false
	}
	for i := range a.Segments {
		if a.Segments[i].ID != b.Segments[i].ID ||
			a.Segments[i].ContentDigest != b.Segments[i].ContentDigest {
			return false
		}
	}
	return true
}

// safePath 拒绝路径逃逸：任何路径段为 ".." 即拒绝。
func safePath(p string) bool {
	if p == "" {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	for _, el := range strings.Split(cleaned, "/") {
		if el == ".." {
			return false
		}
	}
	return true
}
