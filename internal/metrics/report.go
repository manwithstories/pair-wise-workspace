// Package metrics aggregates processing events into the report that the CLI
// prints as JSON. The report is designed to be independently re-checkable:
// every aggregate carries the counts and percentiles needed to recompute it.
package metrics

import (
	"math"
	"sort"
	"sync"

	"mediaroute/internal/audio"
	"mediaroute/internal/clock"
	merr "mediaroute/internal/errors"
	"mediaroute/internal/stream"
)

// SyncToleranceMS is the maximum permitted cross-track event deviation.
const SyncToleranceMS = 20.0

// SyncSample records one cross-track synchronisation observation with the
// source, edit-mapped and output times needed to verify it.
type SyncSample struct {
	TrackID    uint32         `json:"track_id"`
	Sample     int64          `json:"sample"`
	Source     clock.Rational `json:"source"`
	EditMapped clock.Rational `json:"edit_mapped"`
	Output     clock.Rational `json:"output"`
	Deviation  float64        `json:"deviation_ms"`
	Abnormal   bool           `json:"abnormal"`
}

// Report is the terminal/JSON summary.
type Report struct {
	SyncErrorMSP95     float64       `json:"sync_error_ms_p95"`
	SyncErrorMSMax     float64       `json:"sync_error_ms_max"`
	SyncAbnormalCount  int           `json:"sync_abnormal_count"`
	PacketRecoveryRate float64       `json:"packet_recovery_rate"`
	PacketsReceived    int           `json:"packets_received"`
	PacketsRecovered   int           `json:"packets_recovered"`
	PacketsLost        int           `json:"packets_lost"`
	PacketsDuplicate   int           `json:"packets_duplicate"`
	LatencyMSP95       float64       `json:"latency_ms_p95"`
	LatencyMSMax       float64       `json:"latency_ms_max"`
	IndexedSamples     int           `json:"indexed_samples"`
	ProcessedSamples   int           `json:"processed_samples"`
	IsolatedSamples    int           `json:"isolated_samples"`
	CorruptRanges      int           `json:"corrupt_ranges"`
	IndexRebuilt       bool          `json:"index_rebuilt"`
	CacheHit           bool          `json:"cache_hit"`
	AudioBlocks        int           `json:"audio_blocks"`
	AudioFrames        int64         `json:"audio_frames"`
	OutputLUFS         float64       `json:"output_lufs"`
	TruePeakDBTP       float64       `json:"true_peak_dbtp"`
	GainDB             float64       `json:"gain_db"`
	LoudnessLimited    bool          `json:"loudness_limited"`
	LayoutResolved     string        `json:"layout_resolved"`
	LayoutFallback     bool          `json:"layout_fallback"`
	LayoutReason       string        `json:"layout_reason"`
	IndexSummary       string        `json:"index_summary"`
	ArtifactSummary    string        `json:"artifact_summary"`
	PeakMemoryBytes    int           `json:"peak_memory_bytes"`
	ElapsedMS          float64       `json:"elapsed_ms"`
	SyncSamples        []SyncSample  `json:"sync_samples,omitempty"`
	CorruptSpans       []CorruptSpan `json:"corrupt_spans,omitempty"`
	Warnings           []string      `json:"warnings,omitempty"`
}

// CorruptSpan is a reported isolated interval.
type CorruptSpan struct {
	TrackID     uint32  `json:"track_id"`
	FirstSample int64   `json:"first_sample"`
	LastSample  int64   `json:"last_sample"`
	ByteOffset  int64   `json:"byte_offset"`
	Reason      string  `json:"reason"`
	StartMS     float64 `json:"start_ms"`
	EndMS       float64 `json:"end_ms"`
}

// Collector accumulates events. It is safe for concurrent use.
type Collector struct {
	mu      sync.Mutex
	latency []float64
	syncDev []float64
	sync    []SyncSample
	report  Report
	lufsSum float64
	lufsN   int
	peak    float64
}

// New returns an empty collector.
func New() *Collector {
	return &Collector{peak: math.Inf(-1)}
}

// Event is a stream.Sink implementation.
func (c *Collector) Event(ev stream.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ev.Kind {
	case stream.EventSample:
		c.report.ProcessedSamples++
		c.latency = append(c.latency, float64(ev.Latency.Microseconds())/1000)
	case stream.EventCorrupt:
		c.report.IsolatedSamples++
	case stream.EventAudio:
		if ev.Audio != nil {
			c.report.AudioBlocks++
			c.report.AudioFrames += int64(len(ev.Audio.Samples) / audio.TargetChannels)
			if !math.IsInf(ev.Audio.OutputLUFS, -1) {
				c.lufsSum += ev.Audio.OutputLUFS
				c.lufsN++
			}
			if ev.Audio.TruePeakDBTP > c.peak {
				c.peak = ev.Audio.TruePeakDBTP
			}
			if ev.Audio.Limited {
				c.report.LoudnessLimited = true
			}
			if ev.Audio.GainDB > 0 && c.report.GainDB == 0 {
				c.report.GainDB = ev.Audio.GainDB
			}
			if !math.IsInf(ev.Audio.OutputLUFS, -1) {
				c.report.OutputLUFS = ev.Audio.OutputLUFS
			}
		}
		c.latency = append(c.latency, float64(ev.Latency.Microseconds())/1000)
	case stream.EventSync:
		// Sync events are derived at report time from sample times.
	}
}

// ObserveSync records a cross-track deviation. Deviations above the tolerance
// mark the sample abnormal, but never alter the recorded timestamps.
func (c *Collector) ObserveSync(trackID uint32, sample int64, source clock.Rational, devMS float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncDev = append(c.syncDev, devMS)
	if len(c.sync) < 32 || math.Abs(devMS) > SyncToleranceMS {
		c.sync = append(c.sync, SyncSample{
			TrackID: trackID, Sample: sample, Source: source,
			Deviation: devMS, Abnormal: math.Abs(devMS) > SyncToleranceMS,
		})
	}
}

// ObserveSyncDetail records a sync sample with all three timeline views.
func (c *Collector) ObserveSyncDetail(s SyncSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s.Abnormal = math.Abs(s.Deviation) > SyncToleranceMS
	c.syncDev = append(c.syncDev, s.Deviation)
	c.sync = append(c.sync, s)
}

// Finalize fills percentiles and joins the run results.
func (c *Collector) Finalize(res *stream.Result, ix interface {
	Samples() int
	CorruptCount() int
	Summary() string
}, rebuilt, cacheHit bool, spans []CorruptSpan) Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.report
	r.SyncErrorMSP95 = percentile(c.syncDev, 95)
	r.SyncErrorMSMax = maxAbs(c.syncDev)
	for i := range c.sync {
		if math.Abs(c.sync[i].Deviation) > SyncToleranceMS {
			c.sync[i].Abnormal = true
			r.SyncAbnormalCount++
		}
	}
	r.SyncSamples = c.sync
	r.LatencyMSP95 = percentile(c.latency, 95)
	r.LatencyMSMax = maxOf(c.latency)
	if ix != nil {
		r.IndexedSamples = ix.Samples()
		r.CorruptRanges = ix.CorruptCount()
		r.IndexSummary = ix.Summary()
	}
	r.IndexRebuilt = rebuilt
	r.CacheHit = cacheHit
	r.CorruptSpans = spans
	if res != nil {
		r.ProcessedSamples = res.Processed
		r.IsolatedSamples = res.Isolated
		r.PeakMemoryBytes = res.PeakMemory
		r.ElapsedMS = float64(res.Elapsed.Microseconds()) / 1000
		r.PacketsReceived = res.RTPStats.Received
		r.PacketsRecovered = res.RTPStats.Recovered
		r.PacketsLost = res.RTPStats.Lost
		r.PacketsDuplicate = res.RTPStats.Duplicates
		r.PacketRecoveryRate = res.RTPStats.RecoveryRate()
		r.LayoutResolved = res.Layout.Resolved
		r.LayoutFallback = res.Layout.Fallback
		r.LayoutReason = res.Layout.Reason
		r.Warnings = append(r.Warnings, res.Warnings...)
	}
	if c.lufsN > 0 {
		r.OutputLUFS = c.lufsSum / float64(c.lufsN)
	}
	if !math.IsInf(c.peak, -1) {
		r.TruePeakDBTP = c.peak
	}
	return r
}

// percentile returns the nearest-rank p-th percentile of xs.
func percentile(xs []float64, p int) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	rank := int(math.Ceil(float64(p) / 100 * float64(len(cp))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(cp) {
		rank = len(cp)
	}
	return cp[rank-1]
}

func maxAbs(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		if v := math.Abs(x); v > m {
			m = v
		}
	}
	return m
}

func maxOf(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

// SyncDeviation computes the deviation between a source event and the nearest
// event on another track, given sorted reference times in milliseconds.
func SyncDeviation(sourceMS float64, reference []float64) float64 {
	if len(reference) == 0 {
		return 0
	}
	idx := sort.SearchFloat64s(reference, sourceMS)
	best := math.Inf(1)
	for _, i := range []int{idx - 1, idx} {
		if i >= 0 && i < len(reference) {
			if d := math.Abs(reference[i] - sourceMS); d < best {
				best = d
			}
		}
	}
	if math.IsInf(best, 1) {
		return 0
	}
	return best
}

// RTPThreshold is the documented minimum recovery rate for in-window reorder.
const RTPThreshold = 0.95

// Check reports failures as model errors for the CLI exit code. It returns nil
// when every published threshold is met.
func (r Report) Check() error {
	if r.SyncErrorMSP95 > SyncToleranceMS {
		return merr.New(merr.KindTimeline, 0, -1, -1, true, "sync p95 %.2f ms exceeds %.0f ms", r.SyncErrorMSP95, SyncToleranceMS)
	}
	if r.PacketsReceived > 0 && r.PacketRecoveryRate < RTPThreshold && r.PacketsRecovered+r.PacketsLost > 0 {
		return merr.New(merr.KindRTP, 0, -1, -1, true, "packet recovery rate %.3f below %.2f", r.PacketRecoveryRate, RTPThreshold)
	}
	return nil
}
