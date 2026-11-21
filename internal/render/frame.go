// Package render turns a mixed, frame-aligned sample buffer into an offline,
// deterministic frame sequence: per-frame FNV-1a hashes, per-track sample
// counts and MP4 timeline timestamps. Same input and parameters always hash
// identically because no wall clock or map iteration is involved.
package render

import (
	"encoding/binary"
	"hash/fnv"
	"sort"
	"strings"

	"avcheck/internal/clock"
	"avcheck/internal/timeline"
)

// Frame is one 30 fps entry of the offline sequence.
type Frame struct {
	Index        int
	Tick         int64  // start tick on the unified sample timeline
	MP4Timestamp uint64 // composition timestamp in mp4 timescale units
	SampleCount  int    // PCM sample frames covered
	Hash         uint64 // FNV-1a over the frame's exact sample bytes
}

// Sequence is the full deterministic rendering of one mix.
type Sequence struct {
	Frames         []Frame
	FrameHash      uint64 // FNV-1a chain across every frame hash
	PerTrackFrames map[uint32]int
	TotalFrames    int
}

// Renderer produces sequences at a fixed MP4 timescale.
type Renderer struct {
	mp4Timescale uint32
}

// NewRenderer builds a renderer; timescale 0 falls back to the sample rate.
func NewRenderer(mp4Timescale uint32) *Renderer {
	if mp4Timescale == 0 {
		mp4Timescale = clock.SampleRate
	}
	return &Renderer{mp4Timescale: mp4Timescale}
}

// Render splits the mix into 30 fps frames and hashes each one.
func (r *Renderer) Render(m *timeline.Mixed) *Sequence {
	perFrame := int(clock.TicksPerFrame())
	count := m.Frames / perFrame
	seq := &Sequence{
		Frames:         make([]Frame, 0, count),
		PerTrackFrames: m.PerTrackFrames,
		TotalFrames:    m.Frames,
	}
	chain := fnv.New64a()
	var chainBuf [8]byte
	for i := 0; i < count; i++ {
		start := i * perFrame
		end := start + perFrame
		segment := m.Samples[start*m.Channels : end*m.Channels]
		h := fnv.New64a()
		// Write samples as fixed little-endian bytes for stable hashing.
		var buf [4]byte
		for _, s := range segment {
			binary.LittleEndian.PutUint32(buf[:], uint32(s))
			h.Write(buf[:])
		}
		fh := h.Sum64()
		tick := m.StartTick + int64(start)
		f := Frame{
			Index:        i,
			Tick:         tick,
			MP4Timestamp: r.toMP4Time(tick),
			SampleCount:  perFrame,
			Hash:         fh,
		}
		seq.Frames = append(seq.Frames, f)
		binary.LittleEndian.PutUint64(chainBuf[:], fh)
		chain.Write(chainBuf[:])
	}
	seq.FrameHash = chain.Sum64()
	return seq
}

// toMP4Time converts unified sample ticks into the configured MP4 timescale.
func (r *Renderer) toMP4Time(tick int64) uint64 {
	return uint64(tick) * uint64(r.mp4Timescale) / uint64(clock.SampleRate)
}

// Summary renders a compact, deterministic textual digest of the sequence.
func (s *Sequence) Summary() string {
	return frameSummary(s)
}

// frameSummary builds a stable string: frame count, chained hash, sorted
// per-track sample counts and the first/last MP4 timestamps.
func frameSummary(s *Sequence) string {
	var b strings.Builder
	b.WriteString("frames=")
	b.WriteString(itoa(len(s.Frames)))
	b.WriteString(" frame_hash=0x")
	b.WriteString(hex64(s.FrameHash))
	b.WriteString(" total_frames=")
	b.WriteString(itoa(s.TotalFrames))

	ids := make([]uint32, 0, len(s.PerTrackFrames))
	for id := range s.PerTrackFrames {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		b.WriteString(" track")
		b.WriteString(itoa(int(id)))
		b.WriteString("_frames=")
		b.WriteString(itoa(s.PerTrackFrames[id]))
	}
	if len(s.Frames) > 0 {
		b.WriteString(" mp4_first=")
		b.WriteString(utoa(s.Frames[0].MP4Timestamp))
		b.WriteString(" mp4_last=")
		b.WriteString(utoa(s.Frames[len(s.Frames)-1].MP4Timestamp))
	}
	return b.String()
}

func itoa(n int) string { return utoa(uint64(n)) }

func utoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func hex64(n uint64) string {
	const digits = "0123456789abcdef"
	var buf [16]byte
	for i := 15; i >= 0; i-- {
		buf[i] = digits[n&0xF]
		n >>= 4
	}
	return string(buf[:])
}
