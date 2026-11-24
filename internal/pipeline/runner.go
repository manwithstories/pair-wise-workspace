// Package pipeline orchestrates the reusable media delivery kernel: manifest
// parse -> timeline normalization -> demux -> mapping -> bounded streaming
// remux, sharing one rational time base and one error model throughout.
package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"mediadeliver/internal/checkpoint"
	"mediadeliver/internal/index"
	"mediadeliver/internal/manifest"
	"mediadeliver/internal/mapping"
	"mediadeliver/internal/model"
	"mediadeliver/internal/remux"
	"mediadeliver/internal/stream"
	"mediadeliver/internal/verify"
)

// Config controls a delivery run.
type Config struct {
	QueueCapacity  int
	MaxPacketBytes int
	// SlowConsumerDelay sleeps before each output write to force back-pressure.
	SlowConsumerDelay time.Duration
	// CorruptEvery, when >0, fails the Nth output write (1-based) on the first
	// attempt of every segment that has enough packets. The clean retry models
	// recovery from a transient archive write fault. 0 disables injection.
	CorruptEvery int
	// CorruptMaterial flags a material whose source bytes are damaged and must
	// be isolated rather than aborting the whole run.
	CorruptMaterial map[string]bool
}

// Report is the deterministic result of a run.
type Report struct {
	Segments           []*model.SegmentRecord
	ParseConsistency   float64 // % of valid materials parsed successfully
	KeyframeHitRate    float64 // % of cut-point requests served by real keyframes
	BackpressureEvents int64
	PeakBufferBytes    int64
	PeakBufferPackets  int64
	RecoveredSegments  int
	IsolatedTracks     []string
	TrackErrors        []*model.Error
	Checkpoint         checkpoint.State
}

// MaterialSource is one prepared input: its bytes and manifest descriptor.
type MaterialSource struct {
	Material *manifest.Material
	Data     []byte
	Format   string // "mp4" or "rtp"
}

// Runner executes a delivery over in-memory material sources.
type Runner struct {
	cfg    Config
	limits model.Limits
	store  *checkpoint.Store
	cpPath string

	mu          sync.Mutex
	recovered   int
	cut         map[string]*cutStats
	trackErrors []*model.Error
}

// NewRunner builds a Runner with a checkpoint store path.
func NewRunner(cpPath string, cfg Config) *Runner {
	lim := model.DefaultLimits()
	if cfg.MaxPacketBytes > 0 {
		lim.MaxPacketBytes = cfg.MaxPacketBytes
	}
	if cfg.QueueCapacity > 0 {
		lim.MaxQueuePackets = cfg.QueueCapacity
	}
	return &Runner{cfg: cfg, limits: lim, store: checkpoint.NewStore(cpPath), cpPath: cpPath}
}

// Run executes the full data flow and returns a deterministic report.
func (rn *Runner) Run(ctx context.Context, man *manifest.Manifest,
	manifestText []byte, sources map[string]*MaterialSource) (*Report, error) {

	// --- Load / validate checkpoint input digest ---------------------------
	if _, err := rn.store.Load(); err != nil {
		return nil, err
	}
	sourceFiles := map[string]string{}
	for id := range sources {
		sourceFiles[id] = id // identity digest computed over bytes below
	}
	digest := model.InputDigest{
		ManifestSHA256: DigestBytes(manifestText),
		SourceSHA256:   map[string]string{},
	}
	for id, src := range sources {
		digest.SourceSHA256[id] = DigestBytes(src.Data)
	}
	if _, err := rn.store.EnsureInput(digest); err != nil {
		return nil, err
	}

	// --- Demux every material, isolating damaged ones ----------------------
	demuxed := map[string]map[uint32][]*model.Packet{} // material -> track -> packets
	trackInfo := map[string]map[uint32]model.TrackInfo{}
	kfIndex := index.New()
	mapper := mapping.New()

	var validMaterials, totalMaterials int
	var isolated []string
	var trackErrs []*model.Error

	// stable material order for deterministic processing
	matIDs := make([]string, 0, len(sources))
	for id := range sources {
		matIDs = append(matIDs, id)
	}
	sort.Strings(matIDs)

	for _, id := range matIDs {
		src := sources[id]
		totalMaterials++
		if int64(len(src.Data)) > rn.limits.MaxInputLength {
			return nil, model.At(model.KindLimitExceeded, "pipeline", "input length",
				int64(src.Material.Line),
				fmt.Sprintf("material %s is %d bytes, exceeds limit %d",
					id, len(src.Data), rn.limits.MaxInputLength))
		}
		if rn.cfg.CorruptMaterial[id] {
			// A damaged material is detected and isolated; other tracks continue.
			isolated = append(isolated, id)
			trackErrs = append(trackErrs, model.At(model.KindCorruptMedia,
				"pipeline", "demux material", int64(src.Material.Line),
				"material "+id+" marked corrupt; isolated"))
			for _, ts := range src.Material.Tracks {
				mapper.MarkDamaged(id, ts.ID)
			}
			continue
		}
		pkts, infos, err := rn.demuxMaterial(src)
		if err != nil {
			// demux corruption: isolate this material, keep the first position
			isolated = append(isolated, id)
			trackErrs = append(trackErrs, model.Wrap(model.KindCorruptMedia,
				"pipeline", "demux material", model.PositionOf(err),
				"material "+id+" failed demux and was isolated", err))
			for _, ts := range src.Material.Tracks {
				mapper.MarkDamaged(id, ts.ID)
			}
			continue
		}
		validMaterials++
		demuxed[id] = map[uint32][]*model.Packet{}
		trackInfo[id] = map[uint32]model.TrackInfo{}
		for _, inf := range infos {
			trackInfo[id][inf.ID] = inf
			spec, _ := man.FindTrack(id, inf.ID)
			mapper.RegisterTrack(id, inf.ID, inf.Kind, codecOr(spec.Codec, inf.Codec))
		}
		for _, p := range pkts {
			demuxed[id][p.TrackID] = append(demuxed[id][p.TrackID], p)
			kfIndex.Add(p)
		}
	}
	kfIndex.Finalize()
	rn.trackErrors = trackErrs

	// --- Process segments in manifest order ---------------------------------
	var records []*model.SegmentRecord
	var totalBackpressure int64
	var peakBytes, peakPackets int64
	recovered := 0

	for segIdx, segName := range man.SegmentOrder {
		if rn.store.IsComplete(segName) {
			// rerun: already durably delivered; reuse record from checkpoint
			snap := rn.store.Snapshot()
			if rec, ok := snap.Completed[segName]; ok {
				records = append(records, rec)
				continue
			}
		}
		rules := segmentRules(man, segName)
		mappings, err := mapper.Build(segName, rules, true)
		if err != nil {
			return nil, err
		}
		if len(mappings) == 0 {
			// every mapped track was isolated-damaged: skip segment cleanly
			continue
		}

		rec, bp, pBytes, pPackets, didRecover, err := rn.processSegment(
			ctx, segIdx, segName, man, mappings, demuxed, kfIndex)
		if err != nil {
			// A terminal (non-recoverable) failure: discard checkpoint so no
			// partial run is resumable, then propagate.
			_ = rn.store.Discard()
			return nil, err
		}
		records = append(records, rec)
		totalBackpressure += bp
		if pBytes > peakBytes {
			peakBytes = pBytes
		}
		if pPackets > peakPackets {
			peakPackets = pPackets
		}
		if didRecover {
			recovered++
		}
	}

	parseConsistency := 100.0
	if totalMaterials > 0 {
		parseConsistency = float64(validMaterials) / float64(totalMaterials) * 100.0
	}
	snap := rn.store.Snapshot()
	rn.mu.Lock()
	rn.recovered = recovered
	rn.mu.Unlock()

	return &Report{
		Segments:           records,
		ParseConsistency:   parseConsistency,
		KeyframeHitRate:    rn.KeyframeHitRate(),
		BackpressureEvents: totalBackpressure,
		PeakBufferBytes:    peakBytes,
		PeakBufferPackets:  peakPackets,
		RecoveredSegments:  recovered,
		IsolatedTracks:     isolated,
		TrackErrors:        trackErrs,
		Checkpoint:         snap,
	}, nil
}

// processSegment streams one segment through the bounded queue, remuxes it,
// verifies provenance and commits the checkpoint. It retries once on an
// injected recoverable write failure.
func (rn *Runner) processSegment(ctx context.Context, segIdx int, segName string,
	man *manifest.Manifest, mappings []*model.TrackMapping,
	demuxed map[string]map[uint32][]*model.Packet,
	kfIndex *index.KeyframeIndex) (*model.SegmentRecord, int64, int64, int64, bool, error) {

	// Ordered packets for this segment, honoring the edit list.
	packets, err := rn.collectSegmentPackets(man, segName, mappings, demuxed, kfIndex)
	if err != nil {
		return nil, 0, 0, 0, false, err
	}
	if len(packets) == 0 {
		return nil, 0, 0, 0, false, model.At(model.KindInvalidInput,
			"pipeline", "collect segment", -1, "segment "+segName+" has no packets")
	}

	mappingVals := make([]model.TrackMapping, len(mappings))
	for i, m := range mappings {
		mappingVals[i] = *m
	}

	var lastErr error
	attempts := 2 // one normal attempt + one retry after recoverable failure
	didRecover := false
	for attempt := 0; attempt < attempts; attempt++ {
		failIndex := 0
		// Inject one transient fault on the CorruptEvery-th write of the first
		// attempt, but only when the segment actually has that many packets.
		if rn.cfg.CorruptEvery > 0 && attempt == 0 && len(packets) >= rn.cfg.CorruptEvery {
			failIndex = rn.cfg.CorruptEvery
		}
		writer := NewSlowWriter(rn.cfg.SlowConsumerDelay, failIndex)
		rec, bp, pBytes, pPackets, err := rn.streamAndRemux(ctx, segIdx, segName,
			mappingVals, packets, writer)
		if err == nil {
			// Verify the output against provenance before committing.
			if vErr := rn.verifyRecord(rec, writer.Bytes(), demuxed); vErr != nil {
				return nil, 0, 0, 0, false, vErr
			}
			if cErr := rn.store.Commit(rec); cErr != nil {
				return nil, 0, 0, 0, false, cErr
			}
			if attempt > 0 {
				didRecover = true
			}
			return rec, bp, pBytes, pPackets, didRecover, nil
		}
		lastErr = err
		if model.KindOf(err) != model.KindIO {
			break // non-recoverable: cancel/limit/conflict
		}
		// recoverable IO: the segment was not committed, retry cleanly.
	}
	return nil, 0, 0, 0, false, lastErr
}

// streamAndRemux runs producer -> bounded queue -> slow consumer, returns the
// segment record plus the queue peak/backpressure counters.
func (rn *Runner) streamAndRemux(ctx context.Context, segIdx int, segName string,
	mappings []model.TrackMapping, packets []*model.Packet,
	writer *SlowWriter) (*model.SegmentRecord, int64, int64, int64, error) {

	q := stream.NewQueue(rn.limits.MaxQueuePackets, rn.limits.MaxPacketBytes)
	sw := remux.NewSegmentWriter(segIdx, segName, mappings, writer, rn.limits)

	var consumerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			p, err := q.Pop(ctx)
			if err != nil {
				consumerErr = err
				return
			}
			if p == nil {
				return // EOS
			}
			if err := sw.WritePacket(ctx, p); err != nil {
				consumerErr = err
				q.FailWith(err)
				return
			}
		}
	}()

	// Producer: bounded by the queue; slow consumer blocks here.
	for _, p := range packets {
		if err := q.Push(ctx, p); err != nil {
			q.FailWith(err)
			wg.Wait()
			return nil, 0, 0, 0, err
		}
	}
	q.Close()
	wg.Wait()
	if consumerErr != nil {
		return nil, 0, 0, 0, consumerErr
	}
	rec, err := sw.Close()
	if err != nil {
		return nil, 0, 0, 0, err
	}
	st := q.Stats()
	return rec, st.BackpressureEvents, st.PeakBufferBytes, st.PeakBufferedPackets, nil
}

// verifyRecord re-hashes the output and rebuilds provenance from sources.
func (rn *Runner) verifyRecord(rec *model.SegmentRecord, output []byte,
	demuxed map[string]map[uint32][]*model.Packet) error {
	if err := verify.VerifySegment(rec, bytes.NewReader(output)); err != nil {
		return err
	}
	loader := func(materialID string, trackID uint32) ([]*model.Packet, error) {
		if tr, ok := demuxed[materialID]; ok {
			return tr[trackID], nil
		}
		return nil, model.At(model.KindInvalidInput, "pipeline", "verify source",
			-1, fmt.Sprintf("no source %s/%d", materialID, trackID))
	}
	return verify.VerifyProvenance(rec, loader)
}

// RecoveredCount exposes the number of segments that needed a retry.
func (rn *Runner) RecoveredCount() int {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	return rn.recovered
}

// TrackErrors returns the isolated (non-fatal) material/track errors.
func (rn *Runner) TrackErrors() []*model.Error { return rn.trackErrors }

func segmentRules(man *manifest.Manifest, seg string) []*manifest.MappingRule {
	var out []*manifest.MappingRule
	for _, mp := range man.Mappings {
		if mp.Segment == seg {
			out = append(out, mp)
		}
	}
	return out
}

func codecOr(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
