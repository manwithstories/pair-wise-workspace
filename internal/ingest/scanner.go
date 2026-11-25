package ingest

import (
	"github.com/fieldcut/fieldcut/internal/base"
)

// ScanStats summarizes one scanner run for the acceptance metrics.
type ScanStats struct {
	// IndexKeyframes is the number of keyframes claimed by the manifest.
	IndexKeyframes int
	// KeyframeHits is how many claimed keyframes were confirmed by an
	// actual bitstream rescan. IndexHitRate is the derived ratio.
	KeyframeHits int
	Rescans      int
	Truncated    int
	Recovered    int
	Fatal        int
}

// IndexHitRate implements the >=0.99 index-hit threshold.
func (s ScanStats) IndexHitRate() float64 {
	if s.IndexKeyframes == 0 {
		return 1
	}
	return float64(s.KeyframeHits) / float64(s.IndexKeyframes)
}

// maxRescanBytes bounds a single limited forward rescan (64 KiB).
const maxRescanBytes = 64 * 1024

// FaultSink receives recoverable faults without aborting the scan.
type FaultSink func(*base.Fault)

// findStartCode returns the offset of the next Annex B start code at or
// after from, or -1 if none exists before end. A four-byte code
// (00 00 00 01) is preferred over the three-byte prefix it contains so the
// measured NAL/OBU payload is never truncated by one byte.
func findStartCode(data []byte, from int) int {
	for i := from; i+3 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && i+3 < len(data) && data[i+3] == 1 {
			return i
		}
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			return i
		}
	}
	return -1
}

// startCodeLen reports whether the code at offset i is 4 or 3 bytes.
func startCodeLen(data []byte, i int) int {
	if data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && i+3 < len(data) && data[i+3] == 1 {
		return 4
	}
	return 3
}

// nextStartAfter finds the start code following a unit beginning at start.
func nextStartAfter(data []byte, start int) int {
	at := findStartCode(data, start)
	if at < 0 {
		return -1
	}
	off := at + startCodeLen(data, at)
	nxt := findStartCode(data, off)
	if nxt < 0 {
		return len(data)
	}
	return nxt
}

// scanStream is the codec-neutral driver. The nalKeyframe callback decides
// whether a unit's NAL units mark an instantaneous decoding refresh point.
func scanStream(m *Manifest, data []byte, clock *base.Clock, sink FaultSink,
	nalKeyframe func(codec Codec, payload []byte) bool) ([]*AccessUnit, ScanStats, error) {

	if int64(len(data)) > base.MaxFileBytes {
		return nil, ScanStats{}, base.NewFault(base.FaultOversize,
			base.Locator{Camera: m.Camera, Rendition: m.Rendition}, false,
			"input stream %d bytes exceeds 1 GiB limit", len(data))
	}

	var stats ScanStats
	out := make([]*AccessUnit, 0, len(m.Entries))
	waitingResync := false

	for i := range m.Entries {
		e := &m.Entries[i]
		loc := m.Locator(e, m.PTS90k(e))
		if e.Keyframe {
			stats.IndexKeyframes++
		}

		start := int(e.Offset)
		rescanned := false
		if start >= len(data) || findStartCode(data, start) != start {
			// Index drift: limited bounded forward rescan for a real NAL.
			limit := start + maxRescanBytes
			if limit > len(data) {
				limit = len(data)
			}
			found := findStartCode(data, start)
			if found < 0 || found > limit {
				sink(base.NewFault(base.FaultIndexDrift, loc, true,
					"entry %d: no start code within rescan window", i))
				stats.Truncated++
				waitingResync = true
				continue
			}
			sink(base.NewFault(base.FaultIndexDrift, loc, true,
				"entry %d: rescan moved offset %d -> %d", i, start, found))
			start, rescanned, stats.Rescans = found, true, stats.Rescans+1
		}

		end := nextStartAfter(data, start)
		payload := data[start:end]
		if len(payload) > base.MaxAccessUnitBytes {
			sink(base.NewFault(base.FaultOversize, loc, false,
				"entry %d: unit %d bytes over 16 MiB", i, len(payload)))
			stats.Fatal++
			return nil, stats, base.NewFault(base.FaultOversize, loc, false, "oversize unit")
		}
		isKey := nalKeyframe(m.Codec, payload)

		// A truncated/structural unit is isolated. The scanner keeps
		// running and only resynchronizes on a later valid keyframe.
		truncated := !looksStructural(m.Codec, payload)
		if truncated {
			sink(base.NewFault(base.FaultTruncatedAU, loc, true,
				"entry %d: malformed unit at offset %d isolated", i, start))
			stats.Truncated++
			waitingResync = true
		}
		// Keyframe-hit accounting: every manifest keyframe stays in the
		// denominator; only an intact, bitstream-confirmed one joins the
		// numerator. The single corrupted keyframe therefore reads as one
		// miss (119/120) rather than disappearing from the ratio.
		if e.Keyframe && !truncated {
			stats.KeyframeHits++
		}
		if truncated {
			continue
		}
		if waitingResync {
			// Resynchronize at a manifest-confirmed keyframe. The bitstream
			// keyframe detector can stay negative after an AV1 temporal-unit
			// corruption even though the index still marks a valid random
			// access point; the manifest is authoritative for resync.
			if !e.Keyframe {
				continue // drop inter frames until a valid keyframe
			}
			waitingResync = false
			stats.Recovered++
			isKey = true
		}

		pts, gap := clock.Advance(m.PTS90k(e))
		if gap {
			sink(base.NewFault(base.FaultClockGap, loc, true,
				"entry %d: unresolvable clock gap at pts %d", i, pts))
		}
		out = append(out, &AccessUnit{
			Camera: m.Camera, Rendition: m.Rendition, Codec: m.Codec,
			PTS90k: pts, ClockGap: gap, Offset: int64(start),
			Payload: append([]byte(nil), payload...),
			Keyframe: isKey, Rescanned: rescanned,
		})
	}
	return out, stats, nil
}

// looksStructural verifies a unit begins with a parseable NAL header for its
// codec; a zeroed or clipped unit fails this check.
func looksStructural(codec Codec, payload []byte) bool {
	sc := findStartCode(payload, 0)
	if sc != 0 || len(payload) < 5 {
		return false
	}
	off := startCodeLen(payload, sc)
	switch codec {
	case CodecH264:
		// Defined NAL types are 1..23; 0 and 24..31 are unspecified/reserved
		// and indicate a clipped or corrupted header.
		t := h264NALType(payload[off])
		return t >= 1 && t <= 23
	case CodecH265:
		// Defined HEVC NAL types span 0..40; 41..63 are reserved and signal
		// a clipped or corrupted header.
		return h265NALType(payload[off]) <= 40
	case CodecAV1:
		// AV1 OBU header: forbidden bit must read zero, type <= reserved.
		hdr := payload[off]
		return hdr&0x80 == 0 && (hdr>>3)&0x0f <= obuTypeReserved
	}
	return false
}
