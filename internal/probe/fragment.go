package probe

import (
	"fmt"
	"io"

	"mediad/internal/clock"
	"mediad/internal/fault"
)

// Sample is one trun entry with resolved timing information.
type Sample struct {
	Duration uint32
	Size     uint32
	Flags    uint32
	Sync     bool
}

// Fragment is the structural summary of one moof/mdat pair.
type Fragment struct {
	Sequence   uint32
	TrackID    uint32
	BaseTime   uint64
	Timescale  uint32
	Samples    []Sample
	Duration   uint32 // native ticks summed from samples
	MoofOffset int64
	MdatOffset int64
	MdatSize   int64
	HasMoof    bool
	HasMdat    bool
	Determined bool
}

// StartCanonical returns the fragment start on the canonical clock.
func (f Fragment) StartCanonical() int64 {
	native := clock.Timebase{Timescale: f.Timescale}
	if !native.Valid() {
		return 0
	}
	return native.Convert(int64(f.BaseTime), clock.Default())
}

// DurationCanonical returns the summed sample duration on the canonical
// clock, or 0 when the duration was not determined.
func (f Fragment) DurationCanonical() int64 {
	if !f.Determined || !f.TimescaleValid() {
		return 0
	}
	native := clock.Timebase{Timescale: f.Timescale}
	return native.Convert(int64(f.Duration), clock.Default())
}

// TimescaleValid reports whether sample durations can be converted.
func (f Fragment) TimescaleValid() bool { return f.Timescale > 0 }

// MediaInfo is the probe result for one media segment.
type MediaInfo struct {
	URI        string
	Size       int64
	Fragments  []Fragment
	HasMoof    bool
	HasMdat    bool
	Truncated  bool
	StartTicks int64
	EndTicks   int64
	Determined bool
}

// DefaultTimescale is used when a moof does not carry a usable timescale.
const DefaultTimescale = clock.DefaultTimescale

// ParseMedia inspects a media segment: box bounds, moof/mdat presence,
// tfdt/trun timing and tail truncation. Track timescales come from the init
// segment; missing entries fall back to the tfhd defaults declared locally.
func ParseMedia(r io.ReadSeeker, size int64, uri string, timescales map[uint32]uint32) (*MediaInfo, *fault.List, error) {
	var faults fault.List
	if size < 0 || size > maxScanOffset {
		return nil, &faults, fmt.Errorf("probe: invalid segment size %d", size)
	}
	info := &MediaInfo{URI: uri, Size: size, Determined: true}
	boxes, sf := scanBoxes(r, 0, size, maxBoxes)
	faults.Add(sf...)
	var pending *Fragment
	var lastMdatEnd int64 = -1
	for _, b := range boxes {
		switch b.Type {
		case "moof":
			info.HasMoof = true
			if pending != nil {
				// Previous moof never saw an mdat.
				faults.Add(fault.MustNew(fault.KindMissingMdat,
					fault.Location{Segment: uri, Offset: pending.MoofOffset}, "moof without following mdat", true, false))
				pending.HasMdat = false
				info.Fragments = append(info.Fragments, *pending)
			}
			frag, ff := parseMoof(r, b, uri, timescales)
			faults.Add(ff...)
			if frag != nil {
				pending = frag
			} else {
				pending = &Fragment{MoofOffset: b.Offset, HasMoof: true}
			}
		case "mdat":
			info.HasMdat = true
			if pending == nil {
				faults.Add(fault.MustNew(fault.KindMissingMoof,
					fault.Location{Segment: uri, Offset: b.Offset}, "mdat without preceding moof", true, false))
				continue
			}
			pending.HasMdat = true
			pending.MdatOffset = b.Offset
			pending.MdatSize = b.Size
			info.Fragments = append(info.Fragments, *pending)
			pending = nil
			lastMdatEnd = b.End()
		case "styp", "sidx", "emsg", "prft":
			// Legal top-level boxes between fragments.
		}
		if !b.Complete {
			info.Truncated = true
		}
	}
	if pending != nil {
		faults.Add(fault.MustNew(fault.KindMissingMdat,
			fault.Location{Segment: uri, Offset: pending.MoofOffset}, "trailing moof without mdat", true, false))
		info.Fragments = append(info.Fragments, *pending)
	}
	if lastMdatEnd >= 0 && lastMdatEnd < size {
		faults.Add(fault.MustNew(fault.KindTruncatedTail,
			fault.Location{Segment: uri, Offset: lastMdatEnd, Size: size - lastMdatEnd},
			"bytes after last fragment payload", true, false))
		info.Truncated = true
	}
	if !info.HasMoof {
		faults.Add(fault.MustNew(fault.KindMissingMoof,
			fault.Location{Segment: uri, Offset: 0}, "media segment has no moof", true, false))
	}
	if !info.HasMdat && len(info.Fragments) == 0 {
		faults.Add(fault.MustNew(fault.KindMissingMdat,
			fault.Location{Segment: uri, Offset: 0}, "media segment has no mdat", true, false))
	}
	info.computeTimeline(&faults)
	return info, &faults, nil
}

func (info *MediaInfo) computeTimeline(faults *fault.List) {
	first, last := true, int64(0)
	for _, f := range info.Fragments {
		if !f.Determined || !f.TimescaleValid() {
			info.Determined = false
			continue
		}
		if first {
			info.StartTicks = f.StartCanonical()
			first = false
		}
		last = f.StartCanonical() + f.DurationCanonical()
	}
	if !first {
		info.EndTicks = last
	} else {
		info.Determined = false
	}
}
