// Package sim generates deterministic synthetic inputs: multi-rendition
// RTP with duplicates, reordering, timestamp wraparound and burst loss,
// plus fMP4/emsg, SEI, subtitle and chapter metadata. All samples are
// self-generated; no external files are referenced.
package sim

import (
	"fmt"
	"math/rand"
	"sort"

	"mediaroute/internal/adapter"
)

const (
	// ClockRate is the RTP sample clock for video renditions.
	ClockRate = 90000
	// FrameTicks is the RTP tick increment per 40ms frame.
	FrameTicks = 3600
	// FrameIntervalNs is the frame interval on the unified timeline.
	FrameIntervalNs = 40_000_000
	// DefaultDurationNs is the synthetic stream length.
	DefaultDurationNs = 30_000_000_000
	// BaseTick is the unified origin in RTP ticks: the first tick of the
	// earliest rendition. Chosen a few frames below 2^32 so every
	// rendition's clock wraps within the first frames of the stream.
	BaseTick = 0xFFFFFFF0 - 10800
)

// Renditions are the generated video layers; the last is primary.
var Renditions = []string{"video-360p", "video-480p", "video-720p"}

// PrimaryRendition is the layer segment boundaries are chosen from.
const PrimaryRendition = "video-720p"

// renditionTickOffset staggers rendition clocks by ±12ms (±1080 ticks) so
// cross-layer alignment error is nonzero but under the 40ms threshold.
var renditionTickOffset = map[string]int64{
	"video-360p": 0,
	"video-480p": 2160,
	"video-720p": 1080,
}

// Item is one raw input unit in arrival order.
type Item struct {
	Parser string // registry name: "rtp", "mp4", "sei"
	Track  string
	Data   []byte
	Offset int64
	TimeNs int64 // arrival time, for interleaving
}

// Stream is a complete synthetic input set.
type Stream struct {
	Items      []Item
	Meta       []adapter.Event // subtitles and chapters, already timed
	Keyframes  map[string][]int64
	DurationNs int64
	Bandwidth  []int64
}

// Options controls generation.
type Options struct {
	Seed      int64
	DurationNs int64
	LossBurst int    // consecutive packets dropped on video-480p
	Bandwidth []int64
	Corrupt   string // "truncated-box" corrupts the meta track
}

// Generate builds the deterministic stream for the given options.
func Generate(o Options) *Stream {
	if o.DurationNs <= 0 {
		o.DurationNs = DefaultDurationNs
	}
	rng := rand.New(rand.NewSource(o.Seed))
	s := &Stream{
		Keyframes:  map[string][]int64{},
		DurationNs: o.DurationNs,
		Bandwidth:  o.Bandwidth,
	}
	frames := int(o.DurationNs / FrameIntervalNs)
	lossStart := 310 // frame index of the burst, away from keyframes
	for _, track := range Renditions {
		offset := renditionTickOffset[track]
		var units [][]byte
		var times []int64
		for f := 0; f < frames; f++ {
			if track == "video-480p" && o.LossBurst > 0 &&
				f >= lossStart && f < lossStart+o.LossBurst {
				continue // burst loss: packet never arrives
			}
			keyframe := f%25 == 0
			tick := uint32(int64(BaseTick) + offset + int64(f)*FrameTicks)
			tsNs := (int64(BaseTick) + offset + int64(f)*FrameTicks - BaseTick) * 1_000_000_000 / ClockRate
			payload := []byte{0x02, byte(f), byte(f >> 8)}
			if keyframe {
				payload[0] = 0x03
				s.Keyframes[track] = append(s.Keyframes[track], tsNs)
			}
			raw, err := adapter.MarshalPacket(uint16(f), tick, keyframe, payload)
			if err != nil {
				panic(err)
			}
			units = append(units, raw)
			times = append(times, tsNs)
		}
		units, times = injectFaults(units, times)
		var off int64
		for i, raw := range units {
			s.Items = append(s.Items, Item{Parser: "rtp", Track: track, Data: raw, Offset: off, TimeNs: times[i]})
			off += int64(len(raw))
		}
	}
	s.addMetadata(rng, o)
	sort.SliceStable(s.Items, func(i, j int) bool {
		if s.Items[i].TimeNs != s.Items[j].TimeNs {
			return s.Items[i].TimeNs < s.Items[j].TimeNs
		}
		return s.Items[i].Track < s.Items[j].Track
	})
	return s
}

// injectFaults duplicates every 97th unit and swaps adjacent units around
// every 53rd; times follow their units. It returns the grown slices.
func injectFaults(units [][]byte, times []int64) ([][]byte, []int64) {
	for i := len(units) - 1; i > 0; i-- {
		if i%53 == 0 {
			units[i], units[i-1] = units[i-1], units[i]
			times[i], times[i-1] = times[i-1], times[i]
		}
	}
	for i := len(units) - 1; i >= 0; i-- {
		if i%97 == 0 {
			units = append(units, nil)
			times = append(times, 0)
			copy(units[i+1:], units[i:])
			copy(times[i+1:], times[i:])
			units[i+1] = units[i]
			times[i+1] = times[i]
		}
	}
	return units, times
}

// addMetadata appends emsg/ID3 boxes, SEI NALs, subtitles and chapters.
func (s *Stream) addMetadata(rng *rand.Rand, o Options) {
	var offMP4, offSEI int64
	emsgAt := int64(0)
	for t := int64(1_000_000_000); t < o.DurationNs; t += 2_000_000_000 {
		scheme := "urn:example:emsg"
		if emsgAt%2 == 1 {
			scheme = "urn:id3:v2.4"
		}
		box := adapter.BuildEmsg(1000, uint64(t/1_000_000), 100, uint32(emsgAt), scheme, "demo", []byte(fmt.Sprintf("m%d", emsgAt)))
		if o.Corrupt == "truncated-box" && emsgAt == 7 {
			box = box[:len(box)-6] // truncated payload mid-stream
		}
		s.Items = append(s.Items, Item{Parser: "mp4", Track: "meta", Data: box, Offset: offMP4, TimeNs: t})
		offMP4 += int64(len(box))
		emsgAt++
	}
	for t := int64(500_000_000); t < o.DurationNs; t += 1_000_000_000 {
		nal := adapter.BuildSEI(t, []byte("timecode"))
		s.Items = append(s.Items, Item{Parser: "sei", Track: "sei", Data: nal, Offset: offSEI, TimeNs: t})
		offSEI += int64(len(nal))
	}
	for i, t := range []int64{1_000_000_000, 10_000_000_000, 20_000_000_000} {
		s.Meta = append(s.Meta, adapter.Event{
			Track: "chapters", Kind: adapter.KindChapter, Seq: uint32(i),
			TimestampNs: t, Payload: []byte(fmt.Sprintf("chapter-%d", i)),
		})
	}
	for i, t := 0, int64(2_000_000_000); t < o.DurationNs; i, t = i+1, t+2_000_000_000 {
		dur := int64(1_800_000_000)
		if i == 5 {
			dur = -1 // negative duration sample: must be reported, not crash
		}
		s.Meta = append(s.Meta, adapter.Event{
			Track: "subtitles", Kind: adapter.KindSubtitle, Seq: uint32(i),
			TimestampNs: t + rng.Int63n(2_000_000), DurationNs: dur,
			Payload: []byte(fmt.Sprintf("cue-%d", i)),
		})
	}
}
