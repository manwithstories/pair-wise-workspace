// Package hls aligns AV events and cues onto frame-quantized segment
// boundaries shared by every rendition, and emits/verifies manifests.
package hls

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"deliver/internal/cue"
	"deliver/internal/model"
	"deliver/internal/timecode"
)

// Event is a synthetic AV sample on the unified timeline.
type Event struct {
	Seq     int64
	Cam     string // stage, side or roam
	MediaMs model.MediaMs
	Bytes   int
	EnqMs   int64 // wall-clock ms at enqueue, for latency metrics
}

// Size implements pipe.Item.
func (e Event) Size() int { return e.Bytes + 32 }

// Segment is one aligned slice of the timeline.
type Segment struct {
	Index   int
	StartMs model.MediaMs
	EndMs   model.MediaMs
	Cues    []cue.Cue
}

// Boundaries splits [0,totalMs) into segment starts quantized to whole
// frames. All renditions share these boundaries, so cross-rendition
// boundary skew is zero (well under one frame).
func Boundaries(totalMs, segMs int64, fps timecode.FPS) []int64 {
	fn, fd := timecode.FrameDurMs(fps)
	segFrames := (segMs*fd + fn/2) / fn // frames per segment, rounded
	if segFrames < 1 {
		segFrames = 1
	}
	var bounds []int64
	for b := int64(0); b < totalMs; b += timecode.FramesToMs(segFrames, fps) {
		bounds = append(bounds, b)
	}
	return bounds
}

// Segments cuts the timeline and assigns each cue to the single segment
// containing its start, clipped to that segment's end so cross-boundary
// cues are never duplicated.
func Segments(totalMs, segMs int64, fps timecode.FPS, cues []cue.Cue) []Segment {
	bounds := Boundaries(totalMs, segMs, fps)
	segs := make([]Segment, len(bounds))
	for i, b := range bounds {
		end := totalMs
		if i+1 < len(bounds) {
			end = bounds[i+1]
		}
		segs[i] = Segment{Index: i, StartMs: b, EndMs: end}
	}
	for _, c := range cues {
		i := searchSeg(bounds, c.StartMs)
		if i < 0 {
			continue
		}
		if c.EndMs > segs[i].EndMs {
			c.EndMs = segs[i].EndMs // clip at the boundary
		}
		segs[i].Cues = append(segs[i].Cues, c)
	}
	return segs
}

func searchSeg(bounds []int64, ms int64) int {
	lo, hi := 0, len(bounds)-1
	if ms < bounds[0] {
		return -1
	}
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if bounds[mid] <= ms {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// Reassembler deduplicates events by sequence number, reorders
// out-of-order arrivals within a window, and heals gaps from the retry
// cache. Gaps that outlive the window are counted lost.
type Reassembler struct {
	next   int64
	window int
	buf    map[int64]Event
	cache  map[int64]Event // retry cache fed by retransmission
	Dups   int64
	Healed int64
	Lost   int64
}

// NewReassembler expects the stream to start at seq start.
func NewReassembler(start int64, window int) *Reassembler {
	return &Reassembler{next: start, window: window,
		buf: map[int64]Event{}, cache: map[int64]Event{}}
}

// Cache stores a retransmitted packet for gap healing.
func (r *Reassembler) Cache(e Event) { r.cache[e.Seq] = e }

// Push ingests one event and returns everything now in order.
func (r *Reassembler) Push(e Event) []Event {
	if e.Seq < r.next {
		r.Dups++
		return nil
	}
	if _, ok := r.buf[e.Seq]; ok { // duplicate inside the reorder window
		r.Dups++
		return nil
	}
	r.buf[e.Seq] = e
	for len(r.buf) > r.window { // gap outlived the window
		if _, ok := r.buf[r.next]; !ok {
			r.Lost++
			r.next++
		} else {
			break
		}
	}
	return r.drain()
}

// Next returns the next expected sequence number.
func (r *Reassembler) Next() int64 { return r.next }

// Flush declares outstanding gaps lost and drains the buffer at end of
// stream.
func (r *Reassembler) Flush() []Event {
	for len(r.buf) > 0 {
		if _, ok := r.buf[r.next]; ok {
			break
		}
		if _, ok := r.cache[r.next]; ok {
			break
		}
		r.Lost++
		r.next++
	}
	return r.drain()
}

func (r *Reassembler) drain() []Event {
	var out []Event
	for {
		if ev, ok := r.buf[r.next]; ok {
			delete(r.buf, r.next)
			out = append(out, ev)
			r.next++
			continue
		}
		if ev, ok := r.cache[r.next]; ok { // healed from retry cache
			delete(r.cache, r.next)
			r.Healed++
			out = append(out, ev)
			r.next++
			continue
		}
		return out
	}
}

// WriteCheckpoint atomically records the last committed sequence
// (write-temp-then-rename), so a truncated tail never commits twice.
func WriteCheckpoint(path string, seq int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)), 0o644); err != nil {
		return model.Fatalf(model.StageHLS, "checkpoint write: %v", err)
	}
	return os.Rename(tmp, path)
}

// ReadCheckpoint returns the committed sequence, ok=false if none.
func ReadCheckpoint(path string) (int64, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	seq, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false, model.Recoverablef(model.StageHLS, 0, -1,
			"corrupt checkpoint ignored: %v", err)
	}
	return seq, true, nil
}
