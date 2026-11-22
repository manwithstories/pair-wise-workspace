package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"mediapack/internal/cache"
	"mediapack/internal/checkpoint"
	"mediapack/internal/codec"
	"mediapack/internal/fault"
	"mediapack/internal/rtpval"
	"mediapack/internal/track"
)

// Pipeline is one resumable processing run.
type Pipeline struct {
	cfg   Config
	store *cache.Store
	cp    *checkpoint.Manager

	aus        []codec.AccessUnit
	meta       *codec.StreamMeta
	parseCache cachedParse

	current int64 // bytes currently buffered across stages
	peak    int64 // observed high-water mark
	peakMu  sync.Mutex

	packets      atomic.Int64
	revalidated  atomic.Int64
}

// New prepares cache and checkpoint directories under the work directory.
func New(cfg Config) (*Pipeline, error) {
	store, err := cache.NewStore(cfg.WorkDir + "/cache")
	if err != nil {
		return nil, err
	}
	mgr, err := checkpoint.NewManager(cfg.WorkDir + "/state")
	if err != nil {
		return nil, err
	}
	return &Pipeline{cfg: cfg, store: store, cp: mgr}, nil
}

// Manager and Store are exposed for the CLI's verify command.
func (p *Pipeline) Manager() *checkpoint.Manager { return p.cp }
func (p *Pipeline) Store() *cache.Store          { return p.store }

// item is one bounded work unit moving through the goroutine stages.
type item struct {
	au   codec.AccessUnit
	size int
}

// Run executes the full pipeline. A deliberate interruption returns
// *ErrInterrupted with a durable checkpoint already on disk.
func (p *Pipeline) Run(ctx context.Context, input []byte) (*Result, error) {
	if err := p.parseAndMap(ctx, input); err != nil {
		return nil, err
	}
	return p.drive(ctx, input, false)
}

// Resume continues an interrupted run, refusing any input whose identity does
// not match the recorded checkpoint.
func (p *Pipeline) Resume(ctx context.Context, input []byte) (*Result, error) {
	start := time.Now()
	st, ok, err := p.cp.Load()
	if err != nil {
		return nil, err
	}
	if !ok || !st.Committed {
		return nil, fault.New(fault.KindIO, "checkpoint", "no committed checkpoint to resume")
	}
	inputDigest := sha256Hex(input)
	if st.InputSHA256 != inputDigest || st.SourceID != p.cfg.SourceID {
		return nil, fault.New(fault.KindChecksum, "checkpoint",
			"checkpoint identity does not match current input")
	}
	if err := p.parseAndMap(ctx, input); err != nil {
		return nil, err
	}
	resumeMs := time.Since(start).Milliseconds()
	res, err := p.drive(ctx, input, true)
	if err != nil {
		return nil, err
	}
	res.Metrics.ResumeMs = resumeMs
	res.Metrics.Resumed = true
	return res, nil
}

// parseAndMap parses the input, maps the track and warms/validates the cache.
func (p *Pipeline) parseAndMap(ctx context.Context, input []byte) error {
	if err := ctx.Err(); err != nil {
		return fault.New(fault.KindCanceled, "pipeline", "canceled before parse")
	}
	aus, meta, err := codec.ParseBytes(input, 1)
	if err != nil {
		return err
	}
	tr := track.Map(meta.TrackID, track.Video, meta.Codec, aus, nil, nil)
	summary := track.Summarize([]*track.Track{tr})
	p.aus, p.meta = aus, meta
	p.parseCache = cachedParse{
		Codec: meta.Codec.String(), Timeline: summary,
		Keyframes: meta.Keyframes, NumAU: meta.NumAU,
	}
	id := cache.Identity{SourceID: p.cfg.SourceID + "/parse", InputSHA256: sha256Hex(input)}
	if _, hit, err := p.store.Get(id); err != nil {
		return err
	} else if hit {
		p.revalidated.Store(1)
	}
	return p.cacheParse(id, p.parseCache, aus, meta)
}

// fragmentGroups splits access units into chunk-budget-bounded fragments.
func (p *Pipeline) fragmentGroups() [][]codec.AccessUnit {
	var groups [][]codec.AccessUnit
	var cur []codec.AccessUnit
	budget := 0
	for _, au := range p.aus {
		n := au.PayloadLen()
		if len(cur) > 0 && budget+n > p.cfg.ChunkBytes {
			groups = append(groups, cur)
			cur, budget = nil, 0
		}
		cur = append(cur, au)
		budget += n
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

func (p *Pipeline) account(n int) {
	cur := atomic.AddInt64(&p.current, int64(n))
	p.peakMu.Lock()
	if cur > p.peak {
		p.peak = cur
	}
	p.peakMu.Unlock()
}

func (p *Pipeline) release(n int) { atomic.AddInt64(&p.current, -int64(n)) }

var _ = rtpval.Issue(0)
