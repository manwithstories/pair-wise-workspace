// Package segment cuts deinterleaved blocks at nearest keyframe boundaries.
package segment

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"

	"mediaproc/internal/demux"
	"mediaproc/internal/model"
)

// KeyframeRef identifies the keyframe at the start of a segment.
type KeyframeRef struct {
	Index  int
	Offset int64
	TimeNS model.Nanoseconds
}

// Segment is one keyframe-aligned media interval.
type Segment struct {
	Index          int               `json:"segment_index"`
	StartNS        model.Nanoseconds `json:"start_ns"`
	EndNS          model.Nanoseconds `json:"end_ns"`
	KeyframeIndex  int               `json:"keyframe_index"`
	KeyframeOffset int64             `json:"keyframe_offset"`
	KeyframeTimeNS model.Nanoseconds `json:"keyframe_time_ns"`
	Blocks         []*demux.Block    `json:"-"`
	SHA256         string            `json:"sha256"`
	SilentWindows  int               `json:"silent_windows"`
	Fingerprints   []string          `json:"fingerprints,omitempty"`
	MP4Validated   bool              `json:"-"`
}

// TimeRange returns [start,end) in nanoseconds.
func (s *Segment) TimeRange() (model.Nanoseconds, model.Nanoseconds) {
	return s.StartNS, s.EndNS
}

// Options controls keyframe-aligned cutting.
type Options struct {
	// Duration is the desired approximate segment duration.
	Duration model.Nanoseconds
	// TailDuration closes the final segment after its last block.
	TailDuration model.Nanoseconds
}

// DefaultOptions returns 200 ms segments with a 20 ms tail.
func DefaultOptions() Options {
	return Options{Duration: 200 * model.Millisecond, TailDuration: 20 * model.Millisecond}
}

// Cutter accumulates blocks and emits segments when a new keyframe starts.
type Cutter struct {
	opts          Options
	current       *Segment
	keyframeCount int
	segmentCount  int
}

// NewCutter creates a keyframe cutter.
func NewCutter(opts Options) *Cutter {
	if opts.Duration <= 0 {
		opts = DefaultOptions()
	}
	if opts.TailDuration <= 0 {
		opts.TailDuration = 20 * model.Millisecond
	}
	return &Cutter{opts: opts}
}

// Add consumes one block. A keyframe closes the preceding segment when it is
// the nearest boundary to the configured target duration; overdue keyframes
// also close a segment so a missing/skipped keyframe cannot stall cutting.
func (c *Cutter) Add(b *demux.Block) *Segment {
	if b == nil {
		return nil
	}
	if b.Keyframe {
		kf := KeyframeRef{Index: c.keyframeCount, Offset: b.Offset, TimeNS: b.TimeNS}
		c.keyframeCount++
		if c.current == nil {
			c.startNew(kf, b)
			return nil
		}
		if c.shouldCutAt(b.TimeNS) {
			closed := c.current
			c.startNew(kf, b)
			return c.finish(closed, b.TimeNS)
		}
		// An extra keyframe that is not the selected nearest boundary remains
		// in the current segment.
		c.current.Blocks = append(c.current.Blocks, b)
		return nil
	}

	if c.current == nil {
		// Blocks before the first keyframe are still retained in segment zero;
		// this keeps corruption recovery from dropping media.
		kf := KeyframeRef{Index: c.keyframeCount, Offset: b.Offset, TimeNS: b.TimeNS}
		c.keyframeCount++
		c.startNew(kf, b)
	}
	c.current.Blocks = append(c.current.Blocks, b)
	return nil
}

func (c *Cutter) shouldCutAt(t model.Nanoseconds) bool {
	target := c.current.StartNS + c.opts.Duration
	if t >= target {
		return true
	}
	return target-t <= c.opts.Duration/2
}

// NearestKeyframe returns the keyframe closest to target. Ties select the
// earlier keyframe, giving deterministic cuts.
func NearestKeyframe(target model.Nanoseconds, keyframes []KeyframeRef) (KeyframeRef, bool) {
	if len(keyframes) == 0 {
		return KeyframeRef{}, false
	}
	best := keyframes[0]
	bestDist := absNS(target - best.TimeNS)
	for _, kf := range keyframes[1:] {
		d := absNS(target - kf.TimeNS)
		if d < bestDist {
			best, bestDist = kf, d
		}
	}
	return best, true
}

func absNS(v model.Nanoseconds) model.Nanoseconds {
	if v < 0 {
		return -v
	}
	return v
}

func (c *Cutter) startNew(kf KeyframeRef, first *demux.Block) *Segment {
	var closed *Segment
	if c.current != nil {
		closed = c.current
	}
	c.current = &Segment{
		Index:          c.segmentCount,
		StartNS:        kf.TimeNS,
		EndNS:          kf.TimeNS,
		KeyframeIndex:  kf.Index,
		KeyframeOffset: kf.Offset,
		KeyframeTimeNS: kf.TimeNS,
	}
	c.segmentCount++
	if first != nil {
		c.current.Blocks = append(c.current.Blocks, first)
	}
	return closed
}

func (c *Cutter) finish(s *Segment, end model.Nanoseconds) *Segment {
	if end > s.StartNS {
		s.EndNS = end
	}
	s.SHA256 = Hash(s)
	return s
}

// Flush closes the final segment using its last timestamp plus tail duration.
func (c *Cutter) Flush() *Segment {
	if c.current == nil {
		return nil
	}
	end := c.current.StartNS
	if n := len(c.current.Blocks); n > 0 {
		last := c.current.Blocks[n-1].TimeNS
		end = last + c.opts.TailDuration
	} else {
		end += c.opts.TailDuration
	}
	s := c.finish(c.current, end)
	c.current = nil
	return s
}

// KeyframeCount returns keyframes observed so far.
func (c *Cutter) KeyframeCount() int { return c.keyframeCount }

// Hash computes the deterministic SHA-256 of a segment's ordered blocks and
// time range. It includes timestamps/track numbers; fingerprint normalization
// is a separate operation.
func Hash(s *Segment) string {
	h := sha256.New()
	var buf [8]byte
	write := func(v interface{}) {
		switch x := v.(type) {
		case string:
			_, _ = io.WriteString(h, x)
		case []byte:
			_, _ = h.Write(x)
		case int64:
			binary.BigEndian.PutUint64(buf[:], uint64(x))
			_, _ = h.Write(buf[:])
		case uint64:
			binary.BigEndian.PutUint64(buf[:], x)
			_, _ = h.Write(buf[:])
		case int:
			binary.BigEndian.PutUint64(buf[:], uint64(x))
			_, _ = h.Write(buf[:])
		}
	}
	write(int64(s.Index))
	write(int64(s.StartNS))
	write(int64(s.EndNS))
	write(int64(s.KeyframeIndex))
	write(int64(s.KeyframeOffset))
	for _, b := range s.Blocks {
		if b == nil {
			continue
		}
		write(uint64(b.TrackNumber))
		write(int64(b.TimeNS))
		write(int64(b.Offset))
		_, _ = h.Write([]byte{byte(b.Lacing), boolByte(b.Keyframe), boolByte(b.Invisible)})
		write(int(len(b.Frames)))
		for _, f := range b.Frames {
			write(int(len(f)))
			write(f)
		}
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:])
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}
