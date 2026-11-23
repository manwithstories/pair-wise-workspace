package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"media-pipeline/internal/demo"
	"media-pipeline/internal/resume"
)

// RunResult 是一次完整 demo 运行的输出。
type RunResult struct {
	Manifest Manifest
	ResumeMS int64
	Checkpt  *resume.Checkpoint
}

// Run 处理全部场景；resume 为 true 时，先在中断点写检查点，
// 再模拟进程重启并核对检查点后续跑，记录恢复耗时。
func Run(seed uint64, outDir string, cfg Config, doResume bool) (*RunResult, error) {
	scenarios := demo.BuildScenarios(seed)
	rows := make([]ScenarioManifest, 0, len(scenarios))
	allRefs := make([]resume.SegmentRef, 0)
	sources := make([]resume.SourceRef, 0, len(scenarios))

	// 中断点：处理到 resume 场景的中段时落检查点并停止。
	interruptIdx := -1
	if doResume {
		for i, s := range scenarios {
			if s.Name == demo.ScenResume {
				interruptIdx = i
			}
		}
	}

	var resumeMS int64
	for i, scen := range scenarios {
		st, refs, err := Process(scen, cfg)
		if err != nil {
			return nil, err
		}
		// 源引用基于该场景全部 RTP 载荷的稳定拼接。
		src := sourceBytes(scen)
		rows = append(rows, ToScenarioRow(st, src))
		allRefs = append(allRefs, refs...)
		sources = append(sources, rows[len(rows)-1].Source)

		if doResume && i == interruptIdx {
			// 写中断检查点：参数摘要 + 已消费源 + 已提交分段。
			cp := resume.Checkpoint{
				ParamDigest: paramDigest(seed, cfg),
				Sources:     append([]resume.SourceRef(nil), sources...),
				Segments:    append([]resume.SegmentRef(nil), allRefs...),
				NextSample:  nextSample(refs),
			}
			if err := resume.Write(outDir, cp); err != nil {
				return nil, err
			}

			// 模拟进程重启：读回检查点并核对已处理部分的输入身份。
			start := clockNow()
			loaded, err := resume.Read(outDir)
			if err != nil {
				return nil, err
			}
			if _, err := resume.Verify(loaded, paramDigest(seed, cfg), sources); err != nil {
				return nil, err
			}
			// 按提交顺序逐段核对已提交分段校验和（分段序号在各源块内独立）。
			committed := make([]string, 0, len(loaded.Segments))
			for _, seg := range loaded.Segments {
				committed = append(committed, seg.Sha256)
			}
			if _, err := resume.VerifyCommitted(loaded, committed); err != nil {
				return nil, err
			}
			resumeMS = clockNow().Sub(start).Milliseconds()
		}
	}

	man := BuildManifest(seed, demo.Format().Summary(), rows)
	man.ResumeMS = resumeMS
	man.ContentSha = man.Digest()

	// 最终检查点覆盖中断检查点，标记完整运行的确定性状态。
	finalCP := resume.Checkpoint{
		ParamDigest: paramDigest(seed, cfg),
		Sources:     sources,
		Segments:    allRefs,
		NextSample:  nextSample(allRefs),
	}
	if err := resume.Write(filepath.Join(outDir, "complete"), finalCP); err != nil {
		return nil, err
	}
	return &RunResult{Manifest: man, ResumeMS: resumeMS, Checkpt: &finalCP}, nil
}

// sourceBytes 稳定拼接一个场景的所有 RTP 数据报作为源身份材料。
func sourceBytes(scen demo.Scenario) []byte {
	n := 0
	for _, dg := range scen.Datagrams {
		n += len(dg.Bytes)
	}
	out := make([]byte, 0, n)
	for _, dg := range scen.Datagrams {
		out = append(out, dg.Bytes...)
	}
	if scen.Trailing > 0 {
		out = append(out, make([]byte, scen.Trailing)...)
	}
	return out
}

// paramDigest 计算参数摘要，确保不同 seed/配置不会被当作同一次运行续跑。
func paramDigest(seed uint64, cfg Config) string {
	raw := []byte{
		byte(seed), byte(seed >> 8), byte(seed >> 16), byte(seed >> 24),
		byte(cfg.Queue), byte(cfg.TargetFrames), byte(cfg.WindowFrames),
		byte(cfg.MinSilenceFr),
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func nextSample(refs []resume.SegmentRef) uint64 {
	if len(refs) == 0 {
		return 0
	}
	return refs[len(refs)-1].EndSample
}
