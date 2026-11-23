// Package pipeline 编排增量处理：解析→传输→恢复→同步→清单。
// 每个检查点记录输入摘要、处理阶段、输出摘要和来源；
// 恢复时重验缓存对象，任一不符即隔离并从最近可信检查点重算。
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"mediakernel/internal/cache"
	"mediakernel/internal/checkpoint"
	"mediakernel/internal/ebml"
	"mediakernel/internal/fixture"
	"mediakernel/internal/manifest"
	"mediakernel/internal/matroska"
)

// Options 是流水线运行参数。
type Options struct {
	WorkDir       string
	Seed          int64
	LossBurst     int
	BandwidthKbps int
	Resume        bool
}

// Metrics 是恢复质量的量化指标。
type Metrics struct {
	SourceSHA256    string  `json:"source_sha256"`
	Recovered       int     `json:"recovered_packets"`
	Lost            int     `json:"lost_packets"`
	Late            int     `json:"late_packets"`
	TargetKbps      int     `json:"target_kbps"`
	ActualKbps      float64 `json:"actual_kbps"`
	BitrateErrPct   float64 `json:"bitrate_error_pct"`
	PSNRdB          float64 `json:"quality_psnr_db"`
	ManifestStartMs int64   `json:"manifest_start_ms"`
	TimelineErrMs   int64   `json:"timeline_error_ms"`
	CacheRejected   bool    `json:"cache_rejected"`
	Resumed         bool    `json:"checkpoint_resumed"`
}// Result 是流水线输出。
type Result struct {
	Metrics  Metrics           `json:"metrics"`
	Manifest manifest.Manifest `json:"manifest"`
}

// 处理阶段。
const (
	stageParse     = "parse"
	stageTransport = "transport"
	stageManifest  = "manifest"
)

var stageOrder = []string{stageParse, stageTransport, stageManifest}

type runner struct {
	opts   Options
	params fixture.Params

	cache     *cache.Store
	cp        *checkpoint.Store
	srcDigest string
	stages    map[string]string
	rejected  bool
	resumed   bool

	pm  *parsedMedia
	del *delivery
	res *Result
}

// Run 执行流水线；取消、磁盘失败与损坏输入均保留明确状态。
func Run(ctx context.Context, opts Options) (*Result, error) {
	r := &runner{
		opts:   opts,
		stages: map[string]string{},
		params: fixture.Params{
			Seed: opts.Seed, Duration: 2 * time.Second,
			BitrateKbps: opts.BandwidthKbps, LossBurst: opts.LossBurst,
		},
	}
	return r.run(ctx)
}

func (r *runner) run(ctx context.Context) (*Result, error) {
	if err := os.MkdirAll(r.opts.WorkDir, 0o755); err != nil {
		return nil, err
	}
	cs, err := cache.New(filepath.Join(r.opts.WorkDir, "cache"))
	if err != nil {
		return nil, err
	}
	r.cache = cs
	r.cp = &checkpoint.Store{Path: filepath.Join(r.opts.WorkDir, "checkpoint.json")}

	webm, err := r.ensureSource()
	if err != nil {
		return nil, err
	}
	r.srcDigest = cache.Digest(webm)

	startAt := 0
	if r.opts.Resume {
		startAt = r.tryResume()
	}
	if startAt <= 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pm, err := r.doParse(webm)
		if err != nil {
			return nil, err
		}
		r.pm = pm
		if err := r.saveStage(stageParse, pm); err != nil {
			return nil, err
		}
	}
	if startAt <= 1 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.del = r.doTransport(r.pm)
		if err := r.saveStage(stageTransport, r.del); err != nil {
			return nil, err
		}
	}
	if startAt <= 2 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := r.doManifest()
		if err != nil {
			return nil, err
		}
		r.res = res
		if err := r.saveStage(stageManifest, res); err != nil {
			return nil, err
		}
	}
	r.res.Metrics.CacheRejected = r.rejected
	r.res.Metrics.Resumed = r.resumed
	return r.res, r.writeManifest()
}

// ensureSource 读取既有输入；不存在时生成确定性样例。
func (r *runner) ensureSource() ([]byte, error) {
	path := filepath.Join(r.opts.WorkDir, "input.webm")
	data, err := os.ReadFile(path)
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	media := fixture.Generate(r.params)
	if err := os.WriteFile(path, media.WebM, 0o644); err != nil {
		return nil, err
	}
	return media.WebM, nil
}

// tryResume 逐级重验缓存对象来源与摘要，返回最近可信阶段之后的位置。
func (r *runner) tryResume() int {
	cp, ok, err := r.cp.Load()
	if err != nil {
		r.rejected = true // 检查点损坏，不可信
		return 0
	}
	if !ok || cp.Source != r.srcDigest {
		return 0 // 无检查点或输入已变化
	}
	for k, v := range cp.Stages {
		r.stages[k] = v
	}
	target := 0
	for i, st := range stageOrder {
		if st == cp.Stage {
			target = i + 1
		}
	}
	trusted := 0
	for i := 0; i < target; i++ {
		data, err := r.cache.Get(r.stages[stageOrder[i]], r.srcDigest)
		if err != nil {
			r.rejected = true
			break
		}
		if err := r.loadStage(stageOrder[i], data); err != nil {
			r.rejected = true
			break
		}
		trusted = i + 1
	}
	r.resumed = trusted > 0
	return trusted
}

func (r *runner) loadStage(stage string, data []byte) error {
	switch stage {
	case stageParse:
		r.pm = &parsedMedia{}
		return json.Unmarshal(data, r.pm)
	case stageTransport:
		r.del = &delivery{}
		return json.Unmarshal(data, r.del)
	case stageManifest:
		r.res = &Result{}
		return json.Unmarshal(data, r.res)
	}
	return nil
}

// saveStage 缓存阶段输出并原子记录检查点。
func (r *runner) saveStage(stage string, obj any) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	digest, err := r.cache.Put(r.srcDigest, data)
	if err != nil {
		return err
	}
	r.stages[stage] = digest
	return r.cp.Save(checkpoint.Checkpoint{
		Stage: stage, InputDigest: r.srcDigest, OutputDigest: digest,
		Source: r.srcDigest, Stages: r.stages,
	})
}

// doParse 流式解析输入；截断等致命错误带字节偏移返回。
func (r *runner) doParse(webm []byte) (*parsedMedia, error) {
	var blocks []matroska.Block
	var problems []error
	ev := &parseEvents{
		block:   func(b matroska.Block) { blocks = append(blocks, b) },
		problem: func(err error) { problems = append(problems, err) },
	}
	if err := matroska.Parse(ebml.NewReader(bytes.NewReader(webm)), ev); err != nil {
		return nil, err
	}
	for _, p := range problems {
		var te *ebml.TruncatedError
		if errors.As(p, &te) {
			return nil, p
		}
	}
	if len(blocks) == 0 {
		return nil, errors.New("pipeline: no media blocks parsed")
	}
	pm := &parsedMedia{}
	perTrack := map[uint64]int{}
	end := map[uint64]int64{}
	for _, b := range blocks {
		pm.Blocks = append(pm.Blocks, fixture.Block{
			Track: b.Track, Index: perTrack[b.Track], AbsNs: int64(b.Abs),
			Keyframe: b.Keyframe, Data: b.Data,
		})
		perTrack[b.Track]++
		if int64(b.Abs) > end[b.Track] {
			end[b.Track] = int64(b.Abs)
		}
	}
	pm.DurationNs = end[1] + int64(fixture.VideoInterval)
	if a := end[2] + int64(fixture.AudioInterval); a > pm.DurationNs {
		pm.DurationNs = a
	}
	return pm, nil
}

type parseEvents struct {
	block   func(matroska.Block)
	problem func(error)
}
func (e *parseEvents) Tracks([]matroska.Track) {}
func (e *parseEvents) Block(b matroska.Block)  { e.block(b) }
func (e *parseEvents) Problem(err error)       { e.problem(err) }

// writeManifest 把清单原子写入工作目录。
func (r *runner) writeManifest() error {
	path := filepath.Join(r.opts.WorkDir, "manifest.json")
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := manifest.Write(f, r.res.Manifest); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
