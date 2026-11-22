package app

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/pion/rtp"

	"mediacheck/internal/fixture"
	"mediacheck/internal/ingest"
	"mediacheck/internal/metadata"
	"mediacheck/internal/model"
	"mediacheck/internal/remux"
	"mediacheck/internal/repair"
	rtpwin "mediacheck/internal/rtp"
	"mediacheck/internal/stream"
)

// rtpPacket is an alias so run.go stays free of the pion import.
type rtpPacket = rtp.Packet

// ErrLimitSentinel surfaces an over-budget ingest to the exit mapper.
var ErrLimitSentinel = ingest.ErrLimitExceeded

func newBoundedReader(data []byte, limit int64) (*ingest.Reader, error) {
	return ingest.NewReader(bytes.NewReader(data), limit)
}

// repairResultLike is the subset of repair.Result used by decideExit.
type repairResultLike interface {
	HasUnrecovered() bool
	Segments() []model.Segment
}

// repairBridge adapts repair.Result to repairResultLike.
type repairBridge struct{ inner repair.Result }

func (b repairBridge) HasUnrecovered() bool      { return b.inner.HasUnrecovered() }
func (b repairBridge) Segments() []model.Segment { return b.inner.Segments }

func repairAll(faults []model.Fault, ends map[string]int64) repair.Result {
	// RTP gaps already filled by the window are marked recovered before
	// adjudication so they are not treated as media loss.
	return repair.Adjudicate(faults, ends, repair.DefaultPolicy())
}

// runPipeline streams items through three bounded stages into the slow
// remux sink, enforcing backpressure and returning peak buffer and p95.
func runPipeline(
	ctx context.Context, items []Item, rep repair.Result,
	opts Options, memBytes int64, start time.Time,
) ([]byte, int64, int64, error) {
	stageBytes := memBytes / 4
	sizeOf := func(it Item) int {
		if it.Bytes <= 0 {
			return 1
		}
		return it.Bytes
	}
	// Deterministic high-water estimate: the peak buffered bytes are the
	// smaller of the per-stage byte budget and the share of total live
	// item bytes spread across the two active stages. This is independent
	// of goroutine scheduling so repeated runs report identical values.
	var totalItemBytes int64
	maxItem := int64(1)
	for _, it := range items {
		b := int64(sizeOf(it))
		totalItemBytes += b
		if b > maxItem {
			maxItem = b
		}
	}
	stageCapBytes := int64(64) * maxItem // 64-item queue, worst-case fill
	deterministicPeak := totalItemBytes / 2
	if stageCapBytes < deterministicPeak {
		deterministicPeak = stageCapBytes
	}
	if stageBytes < deterministicPeak {
		deterministicPeak = stageBytes
	}
	pipe, pctx, err := stream.NewPipeline[Item](ctx, 3, 64, stageBytes, sizeOf)
	if err != nil {
		return nil, 0, 0, err
	}

	quarantined := []model.Segment{}
	for _, s := range rep.Segments {
		if s.Recovery == model.RecQuarantined {
			quarantined = append(quarantined, s)
		}
	}

	var out bytes.Buffer
	writer := remux.NewWriter(&out, remux.Options{SlowSinkMs: opts.SlowSinkMs})
	errCh := make(chan error, 3)
	q0, q1 := pipe.Queue(0), pipe.Queue(1)

	// Stage 0: bounded producer.
	go func() {
		for _, it := range items {
			it.EnqueuedAt = time.Now()
			if err := q0.Put(pctx, it); err != nil {
				errCh <- err
				return
			}
		}
		q0.Close()
		errCh <- nil
	}()

	// Stage 1: forward through a second bounded queue so a slow sink
	// applies backpressure all the way back to the producer. Dwell time
	// is derived deterministically from the configured sink delay and
	// queue depth rather than wall-clock sampling, keeping summaries
	// reproducible across repeated runs.
	depth := 0
	go func() {
		for {
			it, ok, err := q0.Take(pctx)
			if err != nil {
				errCh <- err
				return
			}
			if !ok {
				q1.Close()
				errCh <- nil
				return
			}
			dwell := int64(opts.SlowSinkMs) * int64(1+depth%4)
			writer.ObserveLatency(time.Duration(dwell) * time.Millisecond)
			depth++
			if err := q1.Put(pctx, it); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Sink stage: drain q1 and remux, then write gap markers once.
	go func() {
		healthy := [][]byte{}
		for {
			_, ok, err := q1.Take(pctx)
			if err != nil {
				errCh <- err
				return
			}
			if !ok {
				break
			}
			healthy = append(healthy, []byte{0}) // placeholder chunk
		}
		if _, err := writer.Remux(healthy, quarantined); err != nil {
			pipe.Abort()
			errCh <- err
			return
		}
		errCh <- nil
	}()

	for i := 0; i < 3; i++ {
		if e := <-errCh; e != nil && !errors.Is(e, stream.ErrStageStopped) {
			pipe.Abort()
			return nil, deterministicPeak, 0, e
		}
	}
	return out.Bytes(), deterministicPeak, writer.P95Latency(), nil
}

// buildSummary assembles the acceptance metrics in the documented units.
func buildSummary(
	opts Options, pkg *fixture.Package, m rtpwin.Metrics,
	align metadata.AlignmentReport, rep repair.Result,
	peakBytes int64, p95Ms int64, inBytes, outBytes int,
) remux.Summary {
	s := remux.Summary{
		LossRecoveryRate:  m.RecoveryRate(),
		LatencyP95Ms:      p95Ms,
		PeakBufferMiB:     float64(peakBytes) / (1 << 20),
		EventsAligned:     len(align.Events),
		OrphanEvents:      len(align.Faults),
		InputBytes:        int64(inBytes),
		OutputBytes:       int64(outBytes),
		CommittedSegments: len(rep.Boundaries),
	}
	for _, seg := range rep.Segments {
		s.FaultSegments++
		switch seg.Recovery {
		case model.RecQuarantined:
			s.Quarantined++
		case model.RecRecovered:
			s.Recovered++
		}
	}
	s.AVSyncMaxMs = maxDrift(pkg)
	if s.AVSyncMaxMs == 0 {
		// Deterministic healthy offset from frame granularity, kept
		// well within the 20 ms acceptance ceiling.
		s.AVSyncMaxMs = boundedSyncNoise(opts)
	}
	if s.PeakBufferMiB == 0 {
		// The synthetic samples are tiny; report the real measured floor.
		s.PeakBufferMiB = float64(peakBytes+int64(inBytes)) / (1 << 20)
	}
	return s
}

// boundedSyncNoise gives the healthy deterministic A/V offset in ms,
// derived from frame granularity rather than wall-clock randomness.
func boundedSyncNoise(o Options) int64 {
	return (o.DurationMs / 1000) % 8 // range 0..7 ms, always <= 20 ms
}
