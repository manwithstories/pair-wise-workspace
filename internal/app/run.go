package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"mediagraph/internal/checkpoint"
	"mediagraph/internal/pipeline"
	"mediagraph/internal/provenance"
	mgrtp "mediagraph/internal/rtp"
	"mediagraph/internal/subtitle"
	"mediagraph/internal/testkit"
)

// ErrCheckpoint 表示检查点或校验失败（映射为退出码 3）。
var ErrCheckpoint = errors.New("checkpoint validation failed")

const (
	manifestName   = "manifest.json"
	provenanceName = "provenance.json"
)

// Demo 生成样例并执行流水线；interruptAfter >= 0 时在完成 N 个步骤后中断。
// 返回摘要与是否中断；中断为预期行为，不视为错误。
func Demo(cfg *pipeline.Config, interruptAfter int) (Summary, bool, error) {
	fx, err := testkit.Generate(cfg.Workdir, cfg.Seed)
	if err != nil {
		return Summary{}, false, err
	}
	return execute(cfg, fx, interruptAfter)
}

// Resume 复用工作目录中的检查点续跑；输入摘要、参数或版本变更拒绝复用。
func Resume(workdir string) (Summary, bool, error) {
	state, err := checkpoint.Load(workdir)
	if err != nil {
		return Summary{}, false, fmt.Errorf("%w: %v", ErrCheckpoint, err)
	}
	if state == nil {
		return Summary{}, false, fmt.Errorf("%w: no checkpoint in %s", ErrCheckpoint, workdir)
	}
	cfg, err := configFromParams(workdir, state.Params)
	if err != nil {
		return Summary{}, false, fmt.Errorf("%w: %v", ErrCheckpoint, err)
	}
	return execute(cfg, testkit.Meta(workdir), -1)
}

// execute 为 demo/resume 的公共路径：校验检查点 → 依赖图执行 →
// 原子持久化检查点、溯源记录与确定性清单。
func execute(cfg *pipeline.Config, fx *testkit.Fixtures, interruptAfter int) (Summary, bool, error) {
	digest, err := checkpoint.DigestInputs(fx.Dir, fx.Files)
	if err != nil {
		return Summary{}, false, err
	}
	state, err := checkpoint.Load(fx.Dir)
	if err != nil {
		return Summary{}, false, fmt.Errorf("%w: %v", ErrCheckpoint, err)
	}
	if state == nil {
		state = checkpoint.New(digest, cfg.Params())
	} else if err := state.Validate(digest, cfg.Params()); err != nil {
		return Summary{}, false, fmt.Errorf("%w: %v", ErrCheckpoint, err)
	}
	plog, err := provenance.Load(filepath.Join(fx.Dir, provenanceName))
	if err != nil {
		return Summary{}, false, err
	}
	ctx := &pipeline.Ctx{Cfg: cfg, State: state, Log: plog}
	b := &builder{fx: fx, cfg: cfg}
	_, interrupted, failed, err := pipeline.Run(ctx, b.steps(), interruptAfter)
	// 无论是否中断都持久化，保证断点可续跑
	if serr := checkpoint.Save(fx.Dir, state); serr != nil && err == nil {
		err = serr
	}
	if perr := plog.Save(filepath.Join(fx.Dir, provenanceName)); perr != nil && err == nil {
		err = perr
	}
	if err != nil {
		return Summary{}, false, err
	}
	for id, ferr := range failed {
		return Summary{}, false, fmt.Errorf("step %s failed: %w", id, ferr)
	}
	sum := summarize(state, plog)
	m := &Manifest{
		Version:        checkpoint.Version,
		Seed:           cfg.Seed,
		Params:         cfg.Params(),
		InputDigest:    digest,
		StepsCompleted: state.Completed,
		Interrupted:    interrupted,
		Summary:        sum,
	}
	if err := saveManifest(filepath.Join(fx.Dir, manifestName), m); err != nil {
		return Summary{}, false, err
	}
	return sum, interrupted, nil
}

// configFromParams 从检查点参数重建运行配置，保证重复提交输出一致。
func configFromParams(workdir string, params map[string]string) (*pipeline.Config, error) {
	cfg := &pipeline.Config{Workdir: workdir, InterruptAfter: -1}
	var err error
	if cfg.Seed, err = strconv.ParseInt(params["seed"], 10, 64); err != nil {
		return nil, fmt.Errorf("bad param seed: %v", err)
	}
	if cfg.WindowMs, err = strconv.ParseInt(params["window_ms"], 10, 64); err != nil {
		return nil, fmt.Errorf("bad param window_ms: %v", err)
	}
	if cfg.MinWindowMs, err = strconv.ParseInt(params["min_win_ms"], 10, 64); err != nil {
		return nil, fmt.Errorf("bad param min_win_ms: %v", err)
	}
	return cfg, nil
}

// saveManifest 原子写入清单（临时文件 + rename）。
func saveManifest(path string, m *Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// VerifyResult 为 verify 子命令的输出。
type VerifyResult struct {
	Fixture          string         `json:"fixture"`
	OK               bool           `json:"ok"` // 无可恢复损坏
	FirstErrorOffset int64          `json:"first_error_offset"`
	FirstErrorFile   string         `json:"first_error_file,omitempty"`
	RecoveredCues    int            `json:"recovered_cues"`
	Issues           map[string]int `json:"issues"` // 文件 → 问题数
}

// Verify 生成样例并校验指定夹具集合（good/corrupt/all），
// 报告首个错误字节偏移与损坏字幕的恢复条数。
func Verify(fixture string, seed int64, workdir string) (VerifyResult, error) {
	if workdir == "" {
		dir, err := os.MkdirTemp("", "mediagraph-verify")
		if err != nil {
			return VerifyResult{}, err
		}
		defer os.RemoveAll(dir)
		workdir = dir
	}
	fx, err := testkit.Generate(workdir, seed)
	if err != nil {
		return VerifyResult{}, err
	}
	res := VerifyResult{Fixture: fixture, OK: true, FirstErrorOffset: -1,
		Issues: map[string]int{}}
	report := func(file string, issues int, first int64, cues int) {
		res.Issues[file] = issues
		res.RecoveredCues += cues
		if issues > 0 {
			res.OK = false
			if res.FirstErrorOffset < 0 {
				res.FirstErrorOffset = first
				res.FirstErrorFile = file
			}
		}
	}
	checkSRT := func(name string) (int, int64, int) {
		data, err := pipeline.ReadInput(fx.Dir, name)
		if err != nil {
			return 1, 0, 0
		}
		r := subtitle.Parse(data)
		first := int64(-1)
		if len(r.Errors) > 0 {
			first = r.Errors[0].Offset
		}
		return len(r.Errors), first, len(r.Cues)
	}
	checkRTP := func(name string) (int, int64, int) {
		data, err := pipeline.ReadInput(fx.Dir, name)
		if err != nil {
			return 1, 0, 0
		}
		r := mgrtp.Ingest(data)
		return len(r.Issues), r.FirstErrorOffset, 0
	}
	checkMP4 := func(name string) (int, int64, int) {
		data, err := pipeline.ReadInput(fx.Dir, name)
		if err != nil {
			return 1, 0, 0
		}
		info := mgrtp.ReadMP4(data)
		return len(info.Issues), info.FirstErrorOffset, 0
	}
	// 固定检查顺序，保证首个错误定位确定
	type check struct {
		file string
		fn   func(string) (int, int64, int)
	}
	var checks []check
	switch fixture {
	case "good":
		checks = []check{{testkit.GoodSRT, checkSRT}, {testkit.GoodVTT, checkSRT},
			{testkit.RTPGood, checkRTP}, {testkit.MP4Good, checkMP4}}
	case "corrupt":
		checks = []check{{testkit.CorruptSRT, checkSRT},
			{testkit.RTPCorrupt, checkRTP}, {testkit.MP4Corrupt, checkMP4}}
	case "all":
		checks = []check{{testkit.GoodSRT, checkSRT}, {testkit.GoodVTT, checkSRT},
			{testkit.CorruptSRT, checkSRT}, {testkit.RTPGood, checkRTP},
			{testkit.RTPCorrupt, checkRTP}, {testkit.MP4Good, checkMP4},
			{testkit.MP4Corrupt, checkMP4}}
	default:
		return VerifyResult{}, fmt.Errorf("unknown fixture %q (want good|corrupt|all)", fixture)
	}
	for _, c := range checks {
		issues, first, cues := c.fn(c.file)
		report(c.file, issues, first, cues)
	}
	return res, nil
}
