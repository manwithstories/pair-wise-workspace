// Package timeline maps raw media timestamps onto the presentation
// timeline: edit-list mapping, rate scaling, 32-bit wrap restoration
// and negative-start clamping. All values are int64 media ticks.
package timeline

import "mp4tool/internal/box"

// TicksToMs converts media ticks to milliseconds (rounded).
func TicksToMs(ticks int64, ts uint32) int64 {
	if ticks >= 0 {
		return (ticks*1000 + int64(ts)/2) / int64(ts)
	}
	return -((-ticks*1000 + int64(ts)/2) / int64(ts))
}

// MsToTicks converts milliseconds to media ticks.
func MsToTicks(ms int64, ts uint32) int64 { return ms * int64(ts) / 1000 }

type segment struct {
	mediaStart   int64 // media ticks
	mediaEnd     int64 // exclusive
	presentStart int64 // presentation ticks (media timescale)
	rate         int32 // 16.16 fixed
}

// Mapper converts raw media PTS to normalized presentation PTS.
type Mapper struct {
	Timescale  uint32
	WrapEvents int
	segs       []segment
	negOffset  int64 // wrap-restored negative offset
	hasSegs    bool
}

// NewMapper builds a Mapper from a track's edit list. movieTS is the
// mvhd timescale (segment durations), mediaTS the mdhd timescale.
// Edit entries with media_time < -1 are treated as 32-bit unsigned
// wrap miswrites: the signed value becomes a global negative offset
// and counts as one wrap event.
func NewMapper(movieTS, mediaTS uint32, elst []box.ElstEntry) *Mapper {
	m := &Mapper{Timescale: mediaTS}
	var presentBase int64
	for _, e := range elst {
		if e.MediaTime < -1 {
			m.negOffset = e.MediaTime
			m.WrapEvents++
			continue
		}
		m.hasSegs = true
		segDurMedia := int64(e.SegmentDuration) * int64(mediaTS) / int64(movieTS)
		if e.Empty() {
			presentBase += segDurMedia
			continue
		}
		rate := e.Rate
		if rate <= 0 {
			rate = 1 << 16
		}
		m.segs = append(m.segs, segment{
			mediaStart:   e.MediaTime,
			mediaEnd:     e.MediaTime + segDurMedia*int64(rate)/(1<<16),
			presentStart: presentBase,
			rate:         rate,
		})
		presentBase += segDurMedia
	}
	return m
}

// Map converts a raw media PTS to normalized presentation PTS.
// The second result is false when the point falls outside every edit
// segment (not presented). Negative results are clamped to 0.
func (m *Mapper) Map(rawPTS int64) (int64, bool) {
	var norm int64
	switch {
	case m.negOffset != 0:
		norm = rawPTS + m.negOffset
	case !m.hasSegs:
		norm = rawPTS
	default:
		for _, s := range m.segs {
			if rawPTS >= s.mediaStart && rawPTS < s.mediaEnd {
				norm = s.presentStart + (rawPTS-s.mediaStart)*(1<<16)/int64(s.rate)
				if norm < 0 {
					norm = 0
				}
				return norm, true
			}
		}
		return 0, false
	}
	if norm < 0 {
		norm = 0
	}
	return norm, true
}
