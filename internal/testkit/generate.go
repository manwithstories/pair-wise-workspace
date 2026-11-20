package testkit

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// Options controls deterministic fixture generation.
type Options struct {
	Tracks        int
	Ladder        int
	DropBurst     int
	TruncateEvery int
	Seed          uint64
}

func (o *Options) normalize() {
	if o.Tracks <= 0 {
		o.Tracks = 4
	}
	if o.Ladder <= 0 {
		o.Ladder = 3
	}
	if o.TruncateEvery <= 0 {
		o.TruncateEvery = 7
	}
}

const (
	segmentSeconds = 4
	durationSeconds = 600
	segmentsPerRep  = durationSeconds / segmentSeconds
)

// SegmentFixture is one generated media segment.
type SegmentFixture struct {
	URI  string
	Data []byte
}

// RepFixture is one representation: a playlist plus its segments.
type RepFixture struct {
	ID        string
	Bandwidth int
	Playlist  []byte
	Segments  []SegmentFixture
}

// TrackFixture is one isolated track with its ladder and RTP stream.
type TrackFixture struct {
	Name      string
	Kind      string // "video" or "audio"
	Timescale int64
	Reps      []RepFixture
	RTP       [][]byte // marshaled packets in arrival order
}

// Case is a full generated scenario.
type Case struct {
	Tracks          []TrackFixture
	CorruptSegments int
}

// Generate builds a deterministic case: nominal 10-minute tracks whose
// fixtures include truncated tails, corrupted length fields, missing
// EXTINF entries, missing keyframes, a 33-bit wrapping timebase, and RTP
// reorder/duplicate/burst-loss damage.
func Generate(opts Options) Case {
	opts.normalize()
	rng := rand.New(rand.NewPCG(opts.Seed, opts.Seed^0x9e3779b97f4a7c15))
	c := Case{}
	for t := 0; t < opts.Tracks; t++ {
		tf := TrackFixture{Name: fmt.Sprintf("track-%d", t)}
		if t%2 == 0 {
			tf.Kind, tf.Timescale = "video", 90000
		} else {
			tf.Kind, tf.Timescale = "audio", 48000
		}
		// Audio tracks start 3-18 ms after video: declared via EXT-X-START
		// and mirrored in the tfdt capture clock, so the pipeline must
		// reconstruct the offset, never silently re-zero it.
		var startOffsetSrc int64
		var startOffsetS string
		if tf.Kind == "audio" {
			ms := 3 + rng.Float64()*15
			startOffsetSrc = int64(ms * float64(tf.Timescale) / 1000)
			startOffsetS = fmt.Sprintf("#EXT-X-START:TIME-OFFSET=%.3f\n", ms/1000)
		}
		// One track runs its timebase through a 33-bit wrap.
		var wrapBase uint64
		if t == 2 {
			wrapBase = (1 << 33) - 30*uint64(tf.Timescale)
		}
		segDurSrc := int64(segmentSeconds) * tf.Timescale
		for r := 0; r < opts.Ladder; r++ {
			rep := RepFixture{
				ID:        fmt.Sprintf("%s-r%d", tf.Name, r),
				Bandwidth: (r + 1) * 400000,
			}
			// Each rung carries a small constant cut offset (<=10 ms)
			// that alignment must report, not absorb.
			rungJitterSrc := int64(r) * 5 * tf.Timescale / 1000
			var pl strings.Builder
			pl.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
			pl.WriteString(startOffsetS)
			for i := 0; i < segmentsPerRep; i++ {
				uri := fmt.Sprintf("%s_seg_%d.m4s", rep.ID, i)
				keyframe := i%5 == 0 && i%23 != 13
				decode := (wrapBase + uint64(startOffsetSrc+rungJitterSrc) +
					uint64(int64(i)*segDurSrc)) & ((1 << 33) - 1)
				payload := make([]byte, 600+r*200+i%97)
				for k := range payload {
					payload[k] = byte(rng.Uint32())
				}
				data := MediaSegment(uint32(i+1), decode, keyframe, payload)
				switch {
				case i%opts.TruncateEvery == 3:
					data = data[:len(data)*3/5] // truncated tail
					c.CorruptSegments++
				case i%opts.TruncateEvery == 5:
					data = CorruptLength(data, "moof", 0x10000)
					c.CorruptSegments++
				}
				rep.Segments = append(rep.Segments, SegmentFixture{URI: uri, Data: data})
				if i%11 != 7 { // every 11th segment loses its EXTINF
					pl.WriteString("#EXTINF:4.000,\n")
				}
				pl.WriteString(uri + "\n")
			}
			pl.WriteString("#EXT-X-ENDLIST\n")
			rep.Playlist = []byte(pl.String())
			tf.Reps = append(tf.Reps, rep)
		}
		tf.RTP = RTPStream(rng, durationSeconds*1000/20, opts.DropBurst, uint32(0x1000+t))
		c.Tracks = append(c.Tracks, tf)
	}
	return c
}
