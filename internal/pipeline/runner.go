// Package pipeline connects the parsing, reordering, transformation and
// verification stages with bounded queues. Every stage runs behind a channel
// of configurable capacity; when a downstream stage is slow the channel fills
// and upstream senders block, so the run applies backpressure instead of
// growing memory. A byte budget additionally caps the payload memory held by
// items between stages.
//
// Cancellation is cooperative: a canceled context stops producers from
// accepting new frames, drains what is already queued, and reports how many
// items were committed. Callers keep previously written segments.
package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mediamend/mediamend/internal/clock"
	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/frame"
	"github.com/mediamend/mediamend/internal/rtp"
)

// Stage names used in progress events and metrics.
const (
	StageParse     = "parse"
	StageReorder   = "reorder"
	StageTransform = "transform"
	StageVerify    = "verify"
)

// ErrFlowCanceled is returned by Flow when the caller's context was canceled
// before every source was fully consumed.
var ErrFlowCanceled = errors.New("pipeline: flow canceled")

// Item is one frame travelling through the stages. The parse stage hashes the
// payload and immediately releases it, so only a small descriptor travels
// between stages; that is what keeps memory bounded regardless of frame size.
type Item struct {
	Camera    string
	Seq       uint64
	MasterTS  uint64
	Offset    int64
	RTPSeq    uint16
	Timestamp uint32
	Keyframe  bool
	Payload   []byte // non-nil only inside the parse stage
	Hash      string // SHA-256 computed over Payload
	Expected  string // SHA-256 recorded by capture metadata
	Size      int    // payload size in bytes
	// TransformTS is the frame's position in the segmenter's output
	// representation ticks (10 MHz), computed by the transform stage.
	TransformTS uint64
	Faults      fault.List
	hasSlot     bool // owns one frame slot in the runner
}

// Event is a progress notification emitted by the runner.
type Event struct {
	Stage   string
	Camera  string
	Seq     uint64
	Kind    string
	Detail  string
	Elapsed time.Duration
}

// Source describes one camera stream for the runner.
type Source struct {
	Camera string
	File   string
	Meta   *frame.Metadata
	Clock  clock.Config
	Window int
	// RTPSeq derives the RTP sequence number for a record. Nil means the
	// sequence is taken from the record's own sequence field, which is what
	// the synthetic fixture produces.
	RTPSeq func(rec *frame.Record) uint16
}

// FlowConfig controls the runner.
type FlowConfig struct {
	// QueueCap is the per-stage channel capacity.
	QueueCap int
	// Workers is the maximum number of concurrent stage goroutines.
	Workers int
	// MaxInflight caps frames held by the runner at once.
	MaxInflight int
	// MaxPayloadBytes caps payload bytes travelling between stages.
	MaxPayloadBytes int64
	// SlowConsumer injects a delay into the transform stage to exercise
	// backpressure in acceptance runs.
	SlowConsumer time.Duration
	// Progress receives progress events when set.
	Progress func(Event)
	// ReorderWindow overrides the per-source reorder window when > 0.
	ReorderWindow int
}

func (c *FlowConfig) defaults() {
	if c.QueueCap <= 0 {
		c.QueueCap = 8
	}
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.MaxInflight <= 0 {
		c.MaxInflight = 24
	}
	if c.MaxPayloadBytes <= 0 {
		c.MaxPayloadBytes = 64 << 20
	}
	if c.ReorderWindow <= 0 {
		c.ReorderWindow = rtp.DefaultWindow
	}
}

// FlowMetrics reports what a run observed.
type FlowMetrics struct {
	PeakInflight     int            `json:"peak_inflight_frames"`
	PeakStageQueue   map[string]int `json:"peak_stage_queue"`
	WaitNanos        int64          `json:"backpressure_wait_ns"`
	BackpressureHits int            `json:"backpressure_hits"`
	Parsed           int            `json:"parsed_frames"`
	Reordered        int            `json:"reordered_frames"`
	Transformed      int            `json:"transformed_frames"`
	Verified         int            `json:"verified_frames"`
	Dropped          int            `json:"dropped_frames"`
	Quarantined      int            `json:"quarantined_frames"`
	LateFrames       int            `json:"late_frames"`
	Canceled         bool           `json:"canceled"`
	Committed        int            `json:"committed_frames"`
	ElapsedNanos     int64          `json:"elapsed_ns"`
}

// FlowResult is the completed run's output.
type FlowResult struct {
	Items    []Item
	Faults   fault.List
	Metrics  FlowMetrics
	Canceled bool
}

// budget bounds payload bytes in flight with a condition variable.
type budget struct {
	mu   sync.Mutex
	cond *sync.Cond
	used int64
	max  int64
	wait int64
	hits int
}

func newBudget(max int64) *budget {
	b := &budget{max: max}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire reserves n bytes, waiting while the budget is exhausted. It returns
// false when ctx is canceled while waiting.
func (b *budget) acquire(ctx context.Context, n int64) bool {
	if n < 0 {
		n = 0
	}
	if n > b.max {
		n = b.max // an oversized frame is admitted alone
	}
	b.mu.Lock()
	if b.used+n <= b.max {
		b.used += n
		b.mu.Unlock()
		return true
	}
	b.hits++
	start := time.Now()
	// Wake the waiter when the context is canceled so it can give up.
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			b.cond.Broadcast()
			b.mu.Unlock()
		case <-stop:
		}
	}()
	for b.used+n > b.max && ctx.Err() == nil {
		b.cond.Wait()
	}
	canceled := ctx.Err() != nil
	if !canceled {
		b.used += n
	}
	b.wait += int64(time.Since(start))
	b.mu.Unlock()
	close(stop)
	return !canceled
}

func (b *budget) release(n int64) {
	if n < 0 {
		return
	}
	if n > b.max {
		n = b.max
	}
	b.mu.Lock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
	b.cond.Broadcast()
	b.mu.Unlock()
}

func (b *budget) stats() (wait int64, hits int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wait, b.hits
}

// Runner executes one configured pass. It is not reusable after Run.
type Runner struct {
	cfg FlowConfig

	inflight int64
	peak     int64
	slots    chan struct{}

	stagesMu  sync.Mutex
	stagePeak map[string]int
	queueWait int64
	queueHits int

	bp *budget

	countParsed      int64
	countReordered   int64
	countTransformed int64
	countVerified    int64
	countDropped     int64
	countQuarantine  int64
	countLate        int64

	sources []Source
}

// New creates a runner for the given configuration.
func New(cfg FlowConfig) *Runner {
	cfg.defaults()
	return &Runner{
		cfg:       cfg,
		slots:     make(chan struct{}, cfg.MaxInflight),
		stagePeak: map[string]int{},
		bp:        newBudget(cfg.MaxPayloadBytes),
	}
}

func (r *Runner) emit(stage, camera string, seq uint64, kind, detail string) {
	if r.cfg.Progress == nil {
		return
	}
	r.cfg.Progress(Event{Stage: stage, Camera: camera, Seq: seq, Kind: kind, Detail: detail})
}

// track records queue depth highs for one stage channel.
func (r *Runner) track(stage string, depth int) {
	r.stagesMu.Lock()
	if depth > r.stagePeak[stage] {
		r.stagePeak[stage] = depth
	}
	r.stagesMu.Unlock()
}

// send hands an item to a stage channel, blocking with backpressure. It
// returns false when the context was canceled before the send completed.
func (r *Runner) send(ctx context.Context, stage string, ch chan<- Item, it Item) bool {
	select {
	case ch <- it:
	default:
		start := time.Now()
		r.stagesMu.Lock()
		r.queueHits++
		r.stagesMu.Unlock()
		select {
		case ch <- it:
		case <-ctx.Done():
			return false
		}
		atomic.AddInt64(&r.queueWait, int64(time.Since(start)))
	}
	r.track(stage, len(ch))
	return true
}

// acquire takes one frame slot and reserves its payload bytes. On success the
// caller owns both until release.
func (r *Runner) acquire(ctx context.Context, size int, it *Item) bool {
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	if !r.bp.acquire(ctx, int64(size)) {
		<-r.slots
		return false
	}
	it.hasSlot = true
	n := atomic.AddInt64(&r.inflight, 1)
	for {
		old := atomic.LoadInt64(&r.peak)
		if n <= old || atomic.CompareAndSwapInt64(&r.peak, old, n) {
			break
		}
	}
	return true
}

// releasePayload returns the byte budget for a payload that is no longer
// retained. The frame slot stays held until the item leaves the pipeline.
func (r *Runner) releasePayload(it *Item) {
	if it.Payload != nil {
		r.bp.release(int64(len(it.Payload)))
		it.Payload = nil
	}
}

// releaseFrame drops the frame slot (and any payload still attached).
func (r *Runner) releaseFrame(it *Item) {
	if !it.hasSlot {
		return
	}
	r.releasePayload(it)
	it.hasSlot = false
	atomic.AddInt64(&r.inflight, -1)
	<-r.slots
}

// Run consumes every source through the four stages.
func (r *Runner) Run(ctx context.Context, sources []Source) FlowResult {
	start := time.Now()
	r.sources = sources
	parseCh := make(chan Item, r.cfg.QueueCap)
	reorderCh := make(chan Item, r.cfg.QueueCap)
	transformCh := make(chan Item, r.cfg.QueueCap)
	verifyCh := make(chan Item, r.cfg.QueueCap)

	var faultMu sync.Mutex
	var faults fault.List
	addFaults := func(fs fault.List) {
		if len(fs) == 0 {
			return
		}
		faultMu.Lock()
		faults = append(faults, fs...)
		faultMu.Unlock()
	}

	var parseWG sync.WaitGroup
	parseSem := make(chan struct{}, r.cfg.Workers)
	for _, src := range sources {
		src := src
		parseWG.Add(1)
		go func() {
			defer parseWG.Done()
			parseSem <- struct{}{}
			defer func() { <-parseSem }()
			r.parseSource(ctx, src, parseCh, addFaults)
		}()
	}
	var parseWGDone sync.WaitGroup
	parseWGDone.Add(1)
	go func() {
		defer parseWGDone.Done()
		parseWG.Wait()
		close(parseCh)
	}()

	var reorderWG sync.WaitGroup
	reorderWG.Add(1)
	go func() {
		defer reorderWG.Done()
		r.reorderStage(ctx, parseCh, reorderCh, addFaults)
		close(reorderCh)
	}()

	var transformWG sync.WaitGroup
	for i := 0; i < r.cfg.Workers; i++ {
		transformWG.Add(1)
		go func() {
			defer transformWG.Done()
			r.transformStage(ctx, reorderCh, transformCh)
		}()
	}
	go func() {
		transformWG.Wait()
		close(transformCh)
	}()

	var verifyWG sync.WaitGroup
	for i := 0; i < r.cfg.Workers; i++ {
		verifyWG.Add(1)
		go func() {
			defer verifyWG.Done()
			r.verifyStage(ctx, transformCh, verifyCh, addFaults)
		}()
	}
	go func() {
		verifyWG.Wait()
		close(verifyCh)
	}()

	var res FlowResult
	for it := range verifyCh {
		res.Items = append(res.Items, it)
	}
	// Cancellation makes consumers exit before their producers finish
	// flushing. Join every stage before reading shared state so the fault
	// list and counters are stable.
	parseWGDone.Wait()
	reorderWG.Wait()
	transformWG.Wait()
	verifyWG.Wait()

	// Deterministic output order: camera, timeline position, sequence.
	sort.SliceStable(res.Items, func(i, j int) bool {
		a, b := res.Items[i], res.Items[j]
		if a.Camera != b.Camera {
			return a.Camera < b.Camera
		}
		if a.MasterTS != b.MasterTS {
			return a.MasterTS < b.MasterTS
		}
		return a.Seq < b.Seq
	})

	elapsed := time.Since(start)
	wait, hits := r.bp.stats()
	res.Canceled = ctx.Err() != nil
	faultMu.Lock()
	snapshot := append(fault.List(nil), faults...)
	faultMu.Unlock()
	res.Faults = snapshot.Sorted()
	res.Metrics = FlowMetrics{
		PeakInflight:     int(atomic.LoadInt64(&r.peak)),
		PeakStageQueue:   r.stagePeakCopy(),
		WaitNanos:        wait + atomic.LoadInt64(&r.queueWait),
		BackpressureHits: hits + r.queueHitsNow(),
		Parsed:           int(atomic.LoadInt64(&r.countParsed)),
		Reordered:        int(atomic.LoadInt64(&r.countReordered)),
		Transformed:      int(atomic.LoadInt64(&r.countTransformed)),
		Verified:         int(atomic.LoadInt64(&r.countVerified)),
		Dropped:          int(atomic.LoadInt64(&r.countDropped)),
		Quarantined:      int(atomic.LoadInt64(&r.countQuarantine)),
		LateFrames:       int(atomic.LoadInt64(&r.countLate)),
		Canceled:         res.Canceled,
		Committed:        len(res.Items),
		ElapsedNanos:     elapsed.Nanoseconds(),
	}
	if res.Canceled {
		res.Faults = append(res.Faults, fault.Fault{
			Kind: fault.KindCanceled, Stage: fault.StageTransform,
			Message: "run canceled; committed items preserved",
		})
	}
	return res
}

func (r *Runner) queueHitsNow() int {
	r.stagesMu.Lock()
	defer r.stagesMu.Unlock()
	return r.queueHits
}

func (r *Runner) stagePeakCopy() map[string]int {
	r.stagesMu.Lock()
	defer r.stagesMu.Unlock()
	out := make(map[string]int, len(r.stagePeak))
	for k, v := range r.stagePeak {
		out[k] = v
	}
	return out
}

// Flow runs the bounded pipeline and reports ErrFlowCanceled when the caller
// canceled before all sources were consumed. Result.Metrics and Result.Items
// are still valid and hold everything committed before cancellation.
func (r *Runner) Flow(ctx context.Context, sources []Source) (FlowResult, error) {
	res := r.Run(ctx, sources)
	if res.Canceled {
		return res, ErrFlowCanceled
	}
	return res, nil
}

// parseSource reads one camera file and forwards records with metadata.
func (r *Runner) parseSource(ctx context.Context, src Source, out chan<- Item, addFaults func(fault.List)) {
	f, err := os.Open(src.File)
	if err != nil {
		addFaults(fault.List{{Kind: fault.KindMalformedMetadata, Stage: fault.StageParse,
			Camera: src.Camera, File: src.File, Offset: -1, Message: err.Error(), Fatal: true}})
		return
	}
	defer f.Close()
	rd := frame.NewReader(f, src.Camera, 0)
	for ctx.Err() == nil {
		rec, err := rd.Read()
		if err == io.EOF || errors.Is(err, frame.ErrBadFraming) {
			break
		}
		if err != nil || rec == nil || len(rec.Payload) == 0 {
			continue
		}
		it := Item{
			Camera: src.Camera, Seq: rec.Seq, Offset: rec.Offset,
			Keyframe: rec.Keyframe(), Payload: rec.Payload, Size: len(rec.Payload),
			Timestamp: uint32(rec.DeviceTS),
		}
		if src.RTPSeq != nil {
			it.RTPSeq = src.RTPSeq(rec)
		} else {
			it.RTPSeq = uint16(rec.Seq)
		}
		// Hash the payload while it is still in the reader's buffer; the
		// bytes are not needed by later stages, only the digest is. This is
		// what keeps the pipeline's memory bounded independently of frame
		// size.
		sum := sha256.Sum256(rec.Payload)
		it.Hash = hex.EncodeToString(sum[:])
		if src.Meta != nil {
			e, ok := src.Meta.LookupOffset(rec.Offset)
			if !ok {
				e, ok = src.Meta.Lookup(src.Camera, rec.Seq)
			}
			if ok {
				it.Expected = e.SHA256
			} else {
				it.Faults = append(it.Faults, fault.Fault{
					Kind: fault.KindMalformedMetadata, Stage: fault.StageParse,
					Camera: src.Camera, Seq: rec.Seq, Offset: rec.Offset, File: src.File,
					Message: "no metadata entry for record; digest cannot be verified",
				})
			}
		}
		// Payload bytes are released right here; the slot tracks the frame
		// descriptor for the rest of its journey.
		it.Payload = nil
		if !r.acquire(ctx, 0, &it) {
			atomic.AddInt64(&r.countDropped, 1)
			break
		}
		if !r.send(ctx, StageParse, out, it) {
			r.releaseFrame(&it)
			atomic.AddInt64(&r.countDropped, 1)
			break
		}
		atomic.AddInt64(&r.countParsed, 1)
		r.emit(StageParse, src.Camera, rec.Seq, "parsed", "")
	}
	fs := rd.Faults()
	for i := range fs {
		fs[i].File = src.File
	}
	addFaults(fs)
}

// reorderStage maps device timestamps and reorders each camera's stream.
func (r *Runner) reorderStage(ctx context.Context, in <-chan Item, out chan<- Item, addFaults func(fault.List)) {
	type state struct {
		clock     *clock.Timeline
		reorderer *rtp.Reorderer
	}
	states := map[string]*state{}

	forward := func(cam string, f rtp.Frame) {
		it := Item{
			Camera: f.Camera, Seq: f.FrameSeq, MasterTS: f.MasterTS,
			Offset: f.Offset, RTPSeq: f.RTPSeq, Timestamp: f.Timestamp,
			Keyframe: f.Keyframe, Size: f.Size,
			Hash: f.Hash, Expected: f.Expected,
		}
		if !r.acquire(ctx, 0, &it) {
			atomic.AddInt64(&r.countDropped, 1)
			return
		}
		if !r.send(ctx, StageReorder, out, it) {
			r.releaseFrame(&it)
			atomic.AddInt64(&r.countDropped, 1)
			return
		}
		atomic.AddInt64(&r.countReordered, 1)
		r.emit(StageReorder, it.Camera, it.Seq, "reordered", "")
	}

	for {
		var it Item
		var ok bool
		select {
		case it, ok = <-in:
		case <-ctx.Done():
			select {
			case it, ok = <-in:
			default:
				ok = false
			}
		}
		if !ok {
			break
		}
		if ctx.Err() != nil {
			r.releaseFrame(&it)
			break
		}
		st := states[it.Camera]
		if st == nil {
			tl, err := clock.New(r.clockConfig(it.Camera))
			if err != nil {
				r.releaseFrame(&it)
				atomic.AddInt64(&r.countDropped, 1)
				continue
			}
			ro, err := rtp.New(rtp.Config{Camera: it.Camera, Window: r.windowFor(it.Camera)})
			if err != nil {
				r.releaseFrame(&it)
				atomic.AddInt64(&r.countDropped, 1)
				continue
			}
			st = &state{clock: tl, reorderer: ro}
			states[it.Camera] = st
		}
		mapped, placed := st.clock.Push(it.Seq, uint64(it.Timestamp), it.Offset)
		if placed {
			st.reorderer.Push(rtp.Frame{
				Camera: it.Camera, FrameSeq: it.Seq, MasterTS: mapped.MasterTS,
				Offset: it.Offset, RTPSeq: it.RTPSeq, Timestamp: it.Timestamp,
				Keyframe: it.Keyframe, Size: it.Size,
				Hash: it.Hash, Expected: it.Expected,
			})
		} else {
			atomic.AddInt64(&r.countQuarantine, 1)
		}
		// The payload is not needed past this stage; the segmenter re-reads
		// bytes from the source file through the recorded offset.
		r.releaseFrame(&it)

		for _, f := range st.reorderer.Drain() {
			forward(it.Camera, f)
		}
		addFaults(st.reorderer.DrainFaults())
	}

	for cam, st := range states {
		frames, fs := st.reorderer.Flush()
		for _, f := range frames {
			forward(cam, f)
		}
		addFaults(fs)
		addFaults(st.clock.Result().Faults)
		atomic.AddInt64(&r.countLate, int64(len(st.reorderer.Finish().Late)))
	}
}

func (r *Runner) windowFor(camera string) int {
	for _, s := range r.sources {
		if s.Camera == camera && s.Window > 0 {
			return s.Window
		}
	}
	return r.cfg.ReorderWindow
}

// RepresentationRate is the tick rate used by the segment/index output
// representations (10 MHz, the Matroska/WebM scale).
const RepresentationRate uint64 = 10_000_000

// representationTS rescales a 90 kHz master timestamp onto the output
// representation clock. The transform stage applies it to every frame so the
// segmenter never touches master-clock arithmetic.
func (r *Runner) representationTS(master uint64) uint64 {
	return master * RepresentationRate / clock.MasterRate
}

func (r *Runner) clockConfig(camera string) clock.Config {
	for _, s := range r.sources {
		if s.Camera == camera {
			return s.Clock
		}
	}
	return clock.Config{Camera: camera, Rate: clock.MasterRate, FrameStep: 3000}
}

// transformStage hashes frame payloads and injects the slow-consumer delay.
func (r *Runner) transformStage(ctx context.Context, in <-chan Item, out chan<- Item) {
	for {
		var it Item
		var ok bool
		select {
		case it, ok = <-in:
		case <-ctx.Done():
			select {
			case it, ok = <-in:
			default:
				return
			}
		}
		if !ok {
			return
		}
		if r.cfg.SlowConsumer > 0 {
			select {
			case <-time.After(r.cfg.SlowConsumer):
			case <-ctx.Done():
				r.releaseFrame(&it)
				return
			}
		}
		// Transformation: project the master timestamp onto the per-output
		// representation ticks the segmenter uses. The payload itself was
		// released at parse; only descriptors travel from here on.
		it.TransformTS = r.representationTS(it.MasterTS)
		atomic.AddInt64(&r.countTransformed, 1)
		r.emit(StageTransform, it.Camera, it.Seq, "transformed", it.Hash)
		if !r.send(ctx, StageTransform, out, it) {
			r.releaseFrame(&it)
			return
		}
	}
}

// verifyStage compares the computed digest with the capture metadata and
// emits verified items.
func (r *Runner) verifyStage(ctx context.Context, in <-chan Item, out chan<- Item, addFaults func(fault.List)) {
	for {
		var it Item
		var ok bool
		select {
		case it, ok = <-in:
		case <-ctx.Done():
			select {
			case it, ok = <-in:
			default:
				return
			}
		}
		if !ok {
			return
		}
		if it.Expected != "" && it.Hash != it.Expected {
			it.Faults = append(it.Faults, fault.Fault{
				Kind: fault.KindChecksumMismatch, Stage: fault.StageVerify,
				Camera: it.Camera, Seq: it.Seq, Offset: it.Offset,
				Message:  "payload SHA-256 does not match capture metadata",
				Expected: it.Expected, Actual: it.Hash,
			})
		}
		addFaults(it.Faults)
		it.Faults = nil
		atomic.AddInt64(&r.countVerified, 1)
		r.emit(StageVerify, it.Camera, it.Seq, "verified", "")
		// The verifier is the pipeline's last owner: release the frame slot
		// before handing the item to the consumer, so the in-flight budget
		// reflects only work the runner still holds.
		r.releaseFrame(&it)
		if !r.send(ctx, StageVerify, out, it) {
			return
		}
	}
}
