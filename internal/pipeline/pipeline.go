// Package pipeline 把适配器、时间线、切分、静音、指纹、日志、清单、核验
// 串成可中断、可续跑的归档流水线。
//
// 故障隔离原则：单个素材的损坏/不兼容/非法剪辑点只隔离该素材，记录后
// 继续处理其余素材，不污染已提交片段。
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"archivemedia/internal/adapter"
	"archivemedia/internal/cut"
	"archivemedia/internal/fingerprint"
	"archivemedia/internal/fixture"
	"archivemedia/internal/journal"
	"archivemedia/internal/manifest"
	"archivemedia/internal/model"
	"archivemedia/internal/timeline"
	"archivemedia/internal/verify"
)

// Config 是流水线参数。
type Config struct {
	Root            string
	InterruptAfter  int // >0 时处理 N 个片段后模拟中断（退出码 3）
	SilenceDb       float64
	SilenceWindow   int
	SilenceMinMs    int64
	MaxBitrateDelta float64
	MaxAlignMs      int64
	MinPsnrDb       float64
}

// DefaultConfig 返回演示用默认参数。
func DefaultConfig(root string) Config {
	return Config{
		Root: root, SilenceDb: -45, SilenceWindow: 400, SilenceMinMs: 100,
		MaxBitrateDelta: 8, MaxAlignMs: 1, MinPsnrDb: 32,
	}
}

// SourceOutcome 记录单个素材的处理结果与故障。
type SourceOutcome struct {
	Kind           string `json:"kind"`
	Path           string `json:"path"`
	Status         string `json:"status"` // ok / isolated / interrupted
	Segments       int    `json:"segments"`
	Recovered      int    `json:"recovered"`
	FirstBadAtUs   int64  `json:"first_bad_at_us,omitempty"`
	ErrorKind      string `json:"error_kind,omitempty"`
	ErrorMessage   string `json:"error_message,omitempty"`
	SilenceCount   int    `json:"silence_intervals"`
	TransitionDedup int   `json:"transition_dedup"`
}

// Result 是一次流水线运行的完整结果。
type Result struct {
	Outcomes       []SourceOutcome
	Report         *verify.Report
	Manifest       manifest.Manifest
	ManifestSha    string
	Interrupted    bool
	Isolated       []SourceOutcome
}

// Runner 持有日志存储与运行状态。
type Runner struct {
	cfg  Config
	jrnl *journal.Store
	seq  int
}

// nextSegmentID 生成全局唯一、确定性的片段 ID。
func (r *Runner) nextSegmentID(kind string) string {
	r.seq++
	return fmt.Sprintf("seg-%s-%03d", kind, r.seq)
}

// New 打开日志存储。
func New(cfg Config) (*Runner, error) {
	j, err := journal.Open(cfg.Root)
	if err != nil {
		return nil, err
	}
	if _, err := manifest.EnsureDir(cfg.Root, "segments"); err != nil {
		return nil, err
	}
	// 续跑时序号从已提交片段之后继续，避免与既有文件碰撞。
	return &Runner{cfg: cfg, jrnl: j, seq: len(j.Committed())}, nil
}

// Run 处理全部素材。
func (r *Runner) Run(sources []fixture.Source) (*Result, error) {
	res := &Result{}
	committedCount := len(r.jrnl.Committed())
	var metrics []verify.SegmentMetric
	var fps []fingerprint.Fingerprint
	var silenceTotal, dedupTotal int

	for si := range sources {
		src := sources[si]
		out := r.processSource(src, &metrics, &fps, &silenceTotal, &dedupTotal, &committedCount)
		res.Outcomes = append(res.Outcomes, out)
		if out.Status == "interrupted" {
			res.Interrupted = true
			// 中断：先持久化已完成工作，再返回可续跑错误。
			r.finishManifest(res, metrics, silenceTotal, dedupTotal)
			return res, model.NewError(model.KindInterrupted,
				"pipeline: simulated interrupt after %d segments", committedCount)
		}
		if out.Status == "isolated" {
			res.Isolated = append(res.Isolated, out)
		}
	}
	r.finishManifest(res, metrics, silenceTotal, dedupTotal)
	return res, nil
}

// processSource 处理单个素材并返回其结局。
func (r *Runner) processSource(src fixture.Source, metrics *[]verify.SegmentMetric,
	fps *[]fingerprint.Fingerprint, silenceTotal, dedupTotal *int,
	committed *int) SourceOutcome {
	out := SourceOutcome{Kind: src.Kind, Path: src.Path, Status: "ok"}

	// 1) 协商适配器并解析。
	parser, err := adapter.Negotiate(src.Path, []adapter.Capability{adapter.CapDemux})
	if err != nil {
		return r.isolate(out, err)
	}
	opts := adapter.DefaultLimits()
	if src.NoDurationLimit {
		opts.MaxDurationUs = 0
	}
	pm, perr := parser.Parse(src.Path, opts)
	if pm != nil && pm.Truncated {
		// 损坏尾包：保留前缀，记录首个坏时间，继续用前缀产出片段。
		out.FirstBadAtUs = pm.FirstBadAt.Microseconds()
		out.ErrorKind = string(model.KindCorruptMedia)
		out.ErrorMessage = perr.Error()
	} else if perr != nil {
		return r.isolate(out, perr) // 不兼容等不可恢复错误：整素材隔离
	}

	// 2) 归一到统一时间轴。
	tl, err := timeline.Normalize(pm)
	if err != nil {
		return r.isolate(out, err)
	}
	engine := cut.NewEngine(tl)

	// 3) 切分普通片段（非法剪辑点逐条隔离，合法的继续）。
	var validRanges []cut.Range
	var illegalCut bool
	for _, pr := range src.Plan.PlanRanges {
		cr := cut.Range{
			TrackIndex: pr.TrackIndex,
			Start:      cut.CutPoint{At: model.MediaTime(pr.StartUs), Mode: pr.Mode},
			End:        cut.CutPoint{At: model.MediaTime(pr.EndUs), Mode: pr.Mode},
		}
		if pr.ExpectInvalid {
			if _, ierr := engine.Cut([]cut.Range{cr}); ierr == nil {
				return r.isolate(out, model.NewError(model.KindInvalidArgument,
					"pipeline: expected-illegal cut was accepted on %s", src.Kind))
			}
			illegalCut = true
			out.ErrorKind = string(model.KindInvalidArgument)
			out.ErrorMessage = "illegal cut point isolated"
			continue
		}
		validRanges = append(validRanges, cr)
	}
	segs, err := engine.Cut(validRanges)
	if err != nil {
		return r.isolate(out, err)
	}

	// 4) 转场复合段（重复声明去重）。
	var tSpecs []cut.TransitionSpec
	for _, tr := range src.Plan.Transitions {
		for k := 0; k < tr.Repeat; k++ {
			tSpecs = append(tSpecs, cut.TransitionSpec{
				TrackA: tr.TrackA, TrackB: tr.TrackB,
				Start: model.MediaTime(tr.StartUs), End: model.MediaTime(tr.EndUs),
				Mode: model.RoundNearest,
			})
		}
	}
	trans, err := engine.ComposeTransitions(tSpecs)
	if err != nil {
		return r.isolate(out, err)
	}
	for _, ts := range trans {
		*dedupTotal += ts.TransitionDedup
	}
	allSegs := append(segs, trans...)

	// 5) 静音检测（音频轨）。
	sil := r.detectSilence(tl, pm)
	*silenceTotal += sil
	out.SilenceCount = sil

	// 6) 逐片段指纹 + 落盘 + 幂等提交。
	for i := range allSegs {
		seg := allSegs[i]
		if err := r.commitSegment(src, pm, tl, seg, parser, metrics, fps, committed); err != nil {
			if model.KindOf(err) == model.KindInterrupted {
				out.Status = "interrupted"
				return out
			}
			return r.isolate(out, err)
		}
		out.Segments++
		out.Recovered++
	}
	switch {
	case out.Status == "interrupted":
	case illegalCut:
		out.Status = "isolated" // 含非法剪辑点的素材整体隔离，合法片段仍已留痕
	case pm.Truncated:
		out.Status = "ok" // 前缀已恢复，整体仍算成功但带损坏标注
	}
	return out
}

// isolate 记录隔离结局，不终止整个流水线。
func (r *Runner) isolate(o SourceOutcome, err error) SourceOutcome {
	o.Status = "isolated"
	o.ErrorKind = string(model.KindOf(err))
	o.ErrorMessage = err.Error()
	return o
}

// finishManifest 用已提交记录构建并原子写出清单，再跑核验汇总。
func (r *Runner) finishManifest(res *Result, metrics []verify.SegmentMetric,
	silence, dupGroups int) {
	committed := r.jrnl.Committed()
	m := manifest.Build(committed, runID(r.cfg.Root), fixedTime)
	sha, err := manifest.WriteAtomically(r.cfg.Root, m)
	if err != nil {
		return
	}
	res.Manifest = m
	res.ManifestSha = sha
	prov := aggregateProvenance(committed)
	th := verify.Thresholds{
		MaxBitrateDeltaPct: r.cfg.MaxBitrateDelta, MaxAlignMs: r.cfg.MaxAlignMs,
		MinPsnrDb: r.cfg.MinPsnrDb,
	}
	rep, _ := verify.Evaluate(metrics, silence, dupGroups, prov, th)
	res.Report = rep
}

// runID 由根目录确定性派生。
func runID(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:16])
}

const fixedTime = "2026-11-27T00:00:00Z"

// aggregateProvenance 把全部片段溯源哈希再聚合为运行级 provenance。
func aggregateProvenance(recs []*journal.SegmentRecord) string {
	keys := make([]string, 0, len(recs))
	for _, rec := range recs {
		keys = append(keys, rec.Provenance)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeJSONLine 供 CLI 输出结构化行。
func writeJSONLine(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}
