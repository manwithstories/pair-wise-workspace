// Package remux rewrites accepted media into a new container while
// preserving an explicit record of every quarantined gap. A slow sink
// is simulated per write so backpressure is observable, and the run
// summary is rendered as stable key/value lines.
package remux

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"time"

	"mediacheck/internal/model"
)

// Summary carries the acceptance metrics printed at the end of a run.
type Summary struct {
	AVSyncMaxMs       int64   `json:"av_sync_max_ms"`
	LossRecoveryRate  float64 `json:"loss_recovery_rate"`
	LatencyP95Ms      int64   `json:"latency_p95_ms"`
	PeakBufferMiB     float64 `json:"peak_buffer_mib"`
	FaultSegments     int     `json:"fault_segments"`
	CommittedSegments int     `json:"committed_segments"`
	Quarantined       int     `json:"quarantined_segments"`
	Recovered         int     `json:"recovered_segments"`
	EventsAligned     int     `json:"events_aligned"`
	OrphanEvents      int     `json:"orphan_events"`
	InputBytes        int64   `json:"input_bytes"`
	OutputBytes       int64   `json:"output_bytes"`
}

// Options configures the writer.
type Options struct {
	// SlowSinkMs sleeps this long per chunk to emulate archive back-pressure.
	SlowSinkMs int
}

// Writer remuxes accepted boxes and writes a manifest of gaps.
type Writer struct {
	out       io.Writer
	opts      Options
	segments  []model.Segment
	latencies []time.Duration
	written   int64
}

// NewWriter wraps a destination writer.
func NewWriter(out io.Writer, opts Options) *Writer {
	return &Writer{out: out, opts: opts}
}

// AcceptedRange marks one timeline span that is safe to remux.
func (w *Writer) AcceptedRange(seg model.Segment) { w.segments = append(w.segments, seg) }

// ObserveLatency records one stage-to-sink latency sample.
func (w *Writer) ObserveLatency(d time.Duration) { w.latencies = append(w.latencies, d) }

// Remux copies the healthy box stream, inserting an explicit free box
// that names each quarantined gap rather than silently closing it.
func (w *Writer) Remux(healthyBoxes [][]byte, quarantined []model.Segment) (int64, error) {
	total := int64(0)
	for _, b := range healthyBoxes {
		if err := w.write(b); err != nil {
			return total, err
		}
		total += int64(len(b))
	}
	// Deterministic gap markers ordered by start time.
	ordered := append([]model.Segment(nil), quarantined...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Track != ordered[j].Track {
			return ordered[i].Track < ordered[j].Track
		}
		return ordered[i].StartMs < ordered[j].StartMs
	})
	for _, seg := range ordered {
		marker := gapMarker(seg)
		if err := w.write(marker); err != nil {
			return total, err
		}
		total += int64(len(marker))
	}
	w.written = total
	return total, nil
}

// write emits one chunk with the configured slow-sink delay.
func (w *Writer) write(chunk []byte) error {
	if w.opts.SlowSinkMs > 0 {
		time.Sleep(time.Duration(w.opts.SlowSinkMs) * time.Millisecond)
	}
	n, err := w.out.Write(chunk)
	if err != nil {
		return fmt.Errorf("remux: sink write failed: %w", err)
	}
	if n != len(chunk) {
		return io.ErrShortWrite
	}
	return nil
}

// gapMarker encodes a 'free' box whose payload records the removed span.
func gapMarker(seg model.Segment) []byte {
	payload := []byte(fmt.Sprintf("GAP track=%s [%d,%d) reason=%s",
		seg.Track, seg.StartMs, seg.EndMs, seg.Reason))
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(8+len(payload)))
	copy(hdr[4:8], []byte("free"))
	return append(hdr, payload...)
}

// P95Latency returns the 95th-percentile observed latency in ms,
// rounding sub-millisecond dwell times up so a real but tiny dwell is
// not reported as zero.
func (w *Writer) P95Latency() int64 {
	if len(w.latencies) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), w.latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (len(sorted) * 95) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	d := sorted[idx]
	ms := d.Milliseconds()
	if ms == 0 && d > 0 {
		ms = 1
	}
	return ms
}

// Render prints the summary as stable key/value lines.
func Render(s Summary) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "av_sync_max_ms %d\n", s.AVSyncMaxMs)
	fmt.Fprintf(&b, "loss_recovery_rate %.4f\n", s.LossRecoveryRate)
	fmt.Fprintf(&b, "latency_p95_ms %d\n", s.LatencyP95Ms)
	fmt.Fprintf(&b, "peak_buffer_mib %.4f\n", s.PeakBufferMiB)
	fmt.Fprintf(&b, "fault_segments %d\n", s.FaultSegments)
	fmt.Fprintf(&b, "committed_segments %d\n", s.CommittedSegments)
	fmt.Fprintf(&b, "quarantined_segments %d\n", s.Quarantined)
	fmt.Fprintf(&b, "recovered_segments %d\n", s.Recovered)
	fmt.Fprintf(&b, "events_aligned %d\n", s.EventsAligned)
	fmt.Fprintf(&b, "orphan_events %d\n", s.OrphanEvents)
	fmt.Fprintf(&b, "input_bytes %d\n", s.InputBytes)
	fmt.Fprintf(&b, "output_bytes %d\n", s.OutputBytes)
	return b.String()
}
