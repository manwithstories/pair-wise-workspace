package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"example.com/medialab/archive/internal/checkpoint"
	"example.com/medialab/archive/internal/ingest"
	"example.com/medialab/archive/internal/ladder"
	"example.com/medialab/archive/internal/metrics"
	"example.com/medialab/archive/internal/model"
	"example.com/medialab/archive/internal/provenance"
	"example.com/medialab/archive/internal/recovery"
	"example.com/medialab/archive/internal/sample"
	"example.com/medialab/archive/internal/segment"
	"example.com/medialab/archive/internal/timeline"
)

// ErrInterrupted is returned when the run stopped at the configured media
// offset. The checkpoint holds a consistent resume point.
var ErrInterrupted = errors.New("pipeline: interrupted at configured offset")

// logEntry is one raw record tagged with its stream index.
type logEntry struct {
	ArrivalUS int64
	Stream    int
	Bytes     []byte
}

// streamState is the live state of one representation's stream.
type streamState struct {
	spec     *streamSpec
	parser   *ingest.Parser
	plan     *sample.Plan
	events   []*model.Event
	dedupe   map[uint32]string
	quarant  map[uint32]model.ErrorKind
	frontier int
	accepted int64
	audioUS  int64
	coverEnd int64
	hasEvent bool
	maxSeq   int64
	// gapCursor is the highest sequence folded into a contiguous run. Holes are
	// accounted exactly once when a later sequence proves them missing.
	gapCursor    int64
	hasGapCursor bool
	// gapEndUS is the media end of the last packet folded into the contiguous
	// run, i.e. where the next hole starts.
	gapEndUS int64
}

// Runner executes one archive run.
type Runner struct {
	cfg      Config
	reps     []model.Representation
	specs    []*streamSpec
	streams  []*streamState
	log      []logEntry
	cursor   int
	wm       *timeline.Watermark
	store    *checkpoint.Store
	manifest *segment.Writer
	ledger   *provenance.Writer

	versions    *timeline.VersionBook
	latest      map[string]model.Segment // newest two slots per rep
	latestSHA   map[string]string
	pending     map[int]map[model.RepresentationID]model.Segment
	segTotal    int
	segCount    int
	counted     map[string]bool
	boundaryMax int64
	skewBySlot  map[string]int64

	syncHist   *metrics.Histogram
	p95Hist    *metrics.Histogram
	quarantine []model.Quarantine
	dupes      int64
	conflicts  int64

	recoveryStats recovery.Stats
	encodedBytes  int64
	outputChain   [32]byte
	outputCount   int

	interrupted bool
	finished    bool

	// materialize optionally writes real fMP4 artefacts for a segment.
	materialize    func(st *streamState, e *segment.Entry) (Materialized, error)
	materializeErr error
	initHashes     map[model.RepresentationID]string
}

// New builds a runner, validating the configuration and regenerating the
// deterministic input log.
func New(cfg Config) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	reps, err := ladder.ParseBitrates(cfg.Bitrates)
	if err != nil {
		return nil, err
	}
	specs, err := streams(cfg, reps)
	if err != nil {
		return nil, err
	}
	if len(specs) < 2 {
		return nil, fmt.Errorf("pipeline: at least two representations are required")
	}
	r := &Runner{
		cfg:       cfg,
		reps:      reps,
		specs:     specs,
		versions:  timeline.NewVersionBook(),
		latest:    map[string]model.Segment{},
		latestSHA: map[string]string{},
		pending:   map[int]map[model.RepresentationID]model.Segment{},
		syncHist:  metrics.NewHistogram(500),
		p95Hist:   metrics.NewHistogram(2000),
	}
	if err := r.build(); err != nil {
		return nil, err
	}
	return r, nil
}

// build regenerates the fault plan and merged arrival log, and resets parsers.
func (r *Runner) build() error {
	var entries []logEntry
	r.streams = nil
	for i, spec := range r.specs {
		plan, err := buildPlan(r.cfg, spec)
		if err != nil {
			return err
		}
		parser, err := ingest.NewParser(spec.cfg)
		if err != nil {
			return err
		}
		st := &streamState{
			spec: spec, parser: parser, plan: plan, maxSeq: -1,
			dedupe: map[uint32]string{}, quarant: map[uint32]model.ErrorKind{},
		}
		r.streams = append(r.streams, st)
		for _, rec := range plan.Records {
			entries = append(entries, logEntry{ArrivalUS: rec.ArrivalUS, Stream: i, Bytes: rec.Bytes})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].ArrivalUS != entries[j].ArrivalUS {
			return entries[i].ArrivalUS < entries[j].ArrivalUS
		}
		return entries[i].Stream < entries[j].Stream
	})
	r.log = entries
	r.wm = timeline.NewWatermark(r.cfg.LateWindowUS)
	r.cursor = 0
	r.segTotal = int((r.cfg.DurationUS + r.cfg.SegmentUS - 1) / r.cfg.SegmentUS)
	r.segCount = 0
	r.boundaryMax = 0
	r.outputChain = [32]byte{}
	r.outputCount = 0
	r.latest = map[string]model.Segment{}
	r.latestSHA = map[string]string{}
	r.pending = map[int]map[model.RepresentationID]model.Segment{}
	r.quarantine = nil
	r.recoveryStats = recovery.Stats{}
	r.skewBySlot = map[string]int64{}
	r.counted = map[string]bool{}
	r.segCount = 0
	r.boundaryMax = 0
	r.encodedBytes = 0
	r.dupes, r.conflicts = 0, 0
	r.interrupted, r.finished = false, false
	return nil
}

// Attach wires the output sinks. Nil sinks still compute all digests, which
// lets the demo compare an interrupted resume against a clean verification run.
func (r *Runner) Attach(store *checkpoint.Store, manifest *segment.Writer, ledger *provenance.Writer) {
	r.store, r.manifest, r.ledger = store, manifest, ledger
}

// Config returns the run configuration.
func (r *Runner) Config() Config { return r.cfg }

// Materializer installs the optional artefact writer.
func (r *Runner) Materializer(fn func(st *streamState, e *segment.Entry) (Materialized, error)) {
	r.materialize = fn
}

// MaterializerError returns the first artefact error, if any.
func (r *Runner) MaterializerError() error { return r.materializeErr }

// Run processes records until the log ends or the configured interruption
// point is reached.
func (r *Runner) Run(ctx context.Context) error {
	for r.cursor < len(r.log) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.process(r.log[r.cursor]); err != nil {
			return err
		}
		r.cursor++
		if r.cfg.InterruptAfterUS > 0 && r.reachedInterrupt() {
			r.interrupted = true
			if err := r.saveCheckpoint(ctx); err != nil {
				return err
			}
			return ErrInterrupted
		}
	}
	r.flush()
	r.settleTail()
	r.finished = true
	return r.saveCheckpoint(ctx)
}

// reachedInterrupt reports whether every stream has covered the offset.
func (r *Runner) reachedInterrupt() bool {
	for _, st := range r.streams {
		if !st.hasEvent || st.coverEnd < r.cfg.InterruptAfterUS {
			return false
		}
	}
	return true
}

// flush commits every segment that is completely covered.
func (r *Runner) flush() {
	for _, st := range r.streams {
		r.commitCovered(st)
		for st.frontier < r.segTotal {
			r.commitSegment(st, st.frontier, false, model.PacketID{}, 0)
			st.frontier++
		}
	}
}

// commitCovered commits segments whose window is fully covered by events.
func (r *Runner) commitCovered(st *streamState) {
	for st.frontier < r.segTotal {
		if st.coverEnd < segEnd(st.frontier, r.cfg.SegmentUS) {
			break
		}
		r.commitSegment(st, st.frontier, false, model.PacketID{}, 0)
		st.frontier++
		r.prune(st, r.cfg.SegmentUS*int64(st.frontier-2))
		r.versions.Forget(st.spec.rep.ID, st.frontier-2)
	}
}

func segStart(index int, segUS int64) int64 { return int64(index) * segUS }
func segEnd(index int, segUS int64) int64   { return int64(index+1) * segUS }

// key is the slot map key.
func key(rep model.RepresentationID, index int) string {
	return fmt.Sprintf("%s/%d", rep, index)
}

// OutputDigest is the deterministic digest of the delivered archive: the
// chained manifest, the chained ledger and the ordered per-segment chain.
func (r *Runner) OutputDigest() string {
	return hex.EncodeToString(r.outputChain[:])
}

// manifestDigest returns the manifest chain for checkpointing.
func (r *Runner) manifestDigest() string {
	if r.manifest == nil {
		return ""
	}
	return r.manifest.ChainHex()
}

// ledgerDigest returns the ledger chain for checkpointing.
func (r *Runner) ledgerDigest() string {
	if r.ledger == nil {
		return ""
	}
	return r.ledger.Digest()
}

// elapsed records one segment build duration for the latency metric.
func (r *Runner) elapsed(start time.Time) {
	r.p95Hist.Observe(time.Since(start).Milliseconds())
}

// chainSegment folds one committed segment into the output chain.
func (r *Runner) chainSegment(e *segment.Entry) {
	h := sha256.New()
	h.Write(r.outputChain[:])
	fmt.Fprintf(h, "%s|%d|%d|%s\n", e.Rep, e.Index, e.Version, e.OutputSHA256)
	copy(r.outputChain[:], h.Sum(nil))
	r.outputCount++
}

// saveCheckpoint persists the resume cursor and the opaque pipeline state.
func (r *Runner) saveCheckpoint(ctx context.Context) error {
	if r.store == nil {
		return nil
	}
	state := checkpoint.State{
		InputOffset:      int64(r.cursor),
		ConfigDigest:     r.cfg.Digest(r.reps),
		Committed:        map[string]int{},
		CommittedSHA:     map[string]string{},
		ManifestChain:    r.manifestDigest(),
		LedgerDigest:     r.ledgerDigest(),
		TimelineVersions: map[string]int{},
		MaxVersion:       r.versions.MaxVersion(),
		ReplayedSegments: r.versions.ReplayedSegments(),
	}
	if r.manifest != nil {
		state.ManifestCount = r.manifest.Count()
	}
	for _, st := range r.streams {
		state.Committed[string(st.spec.rep.ID)] = st.frontier
	}
	for k, sha := range r.latestSHA {
		state.CommittedSHA[k] = sha
	}
	versions, _, _ := r.versions.Export()
	state.TimelineVersions = versions
	payload, err := r.marshalPipelineState()
	if err != nil {
		return err
	}
	state.Pipeline = payload
	return r.store.Save(ctx, state)
}
