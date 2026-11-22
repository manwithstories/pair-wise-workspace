package probe

import (
	"bytes"
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"mediad/internal/fault"
)

func parseMoof(r io.ReadSeeker, moof Box, uri string, timescales map[uint32]uint32) (*Fragment, []fault.Fault) {
	var faults []fault.Fault
	frag := &Fragment{MoofOffset: moof.Offset, HasMoof: true, Determined: true}
	children, cf := scanBoxes(r, moof.PayloadOffset(), moof.End(), 4096)
	faults = append(faults, cf...)
	var traf *Box
	for i := range children {
		switch children[i].Type {
		case "traf":
			if traf == nil {
				traf = &children[i]
			}
		case "mfhd":
			if payload, err := readPayload(r, children[i]); err == nil {
				var mfhd mp4.Mfhd
				if _, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), &mfhd, mp4.Context{}); err == nil {
					frag.Sequence = mfhd.SequenceNumber
				}
			}
		}
	}
	if traf == nil {
		faults = append(faults, fault.MustNew(fault.KindMissingMoof,
			fault.Location{Segment: uri, Offset: moof.Offset}, "moof has no traf", true, false))
		return frag, faults
	}
	trafChildren, tf := scanBoxes(r, traf.PayloadOffset(), traf.End(), 4096)
	faults = append(faults, tf...)
	var tfdtSet bool
	var truns []Box
	var tfhd Box
	var hasTfhd bool
	for _, c := range trafChildren {
		switch c.Type {
		case "tfhd":
			tfhd, hasTfhd = c, true
		case "tfdt":
			if payload, err := readPayload(r, c); err == nil {
				var box mp4.Tfdt
				if _, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), &box, mp4.Context{}); err == nil {
					frag.BaseTime = box.GetBaseMediaDecodeTime()
					tfdtSet = true
				}
			}
		case "trun":
			truns = append(truns, c)
		}
	}
	if !hasTfhd {
		faults = append(faults, fault.MustNew(fault.KindMissingMoof,
			fault.Location{Segment: uri, Offset: traf.Offset}, "traf has no tfhd", true, false))
	}
	var baseTime uint64
	var timescale uint32
	if hasTfhd {
		payload, err := readPayload(r, tfhd)
		if err == nil {
			var box mp4.Tfhd
			if _, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), &box, mp4.Context{}); err == nil {
				frag.TrackID = box.TrackID
				if box.GetFlags()&mp4.TfhdDefaultBaseIsMoof != 0 {
					baseTime = uint64(moof.Offset)
				}
				if ts, ok := timescales[box.TrackID]; ok {
					timescale = ts
				}
			}
		}
	}
	if timescale == 0 {
		// Without an init segment, the local default duration cannot be
		// converted; leave the timeline undetermined rather than guessing.
		frag.Determined = false
	}
	frag.Timescale = timescale
	_ = baseTime
	if !tfdtSet {
		faults = append(faults, fault.MustNew(fault.KindTimestampDiscontinuity,
			fault.Location{Segment: uri, Offset: traf.Offset}, "traf has no tfdt", true, false))
		frag.Determined = false
	}
	for _, trunBox := range truns {
		payload, err := readPayload(r, trunBox)
		if err != nil {
			faults = append(faults, fault.MustNew(fault.KindBoxOverflow,
				fault.Location{Segment: uri, Offset: trunBox.Offset}, err.Error(), true, false))
			frag.Determined = false
			continue
		}
		var trun mp4.Trun
		if _, err := mp4.Unmarshal(bytes.NewReader(payload), uint64(len(payload)), &trun, mp4.Context{}); err != nil {
			faults = append(faults, fault.MustNew(fault.KindBoxHeader,
				fault.Location{Segment: uri, Offset: trunBox.Offset}, "invalid trun payload", true, false))
			frag.Determined = false
			continue
		}
		for i, e := range trun.Entries {
			s := Sample{Duration: e.SampleDuration, Size: e.SampleSize}
			if !trun.CheckFlag(0x100) {
				// Duration inherited from tfhd/trex cannot be confirmed here.
				frag.Determined = false
			}
			if i == 0 && trun.CheckFlag(0x4) {
				s.Flags = trun.FirstSampleFlags
			} else if trun.CheckFlag(0x400) {
				s.Flags = e.SampleFlags
			}
			s.Sync = isSyncSample(s.Flags)
			frag.Samples = append(frag.Samples, s)
			frag.Duration += s.Duration
		}
		if uint32(len(trun.Entries)) != trun.SampleCount {
			faults = append(faults, fault.MustNew(fault.KindBoxHeader,
				fault.Location{Segment: uri, Offset: trunBox.Offset},
				fmt.Sprintf("trun sample_count=%d entries=%d", trun.SampleCount, len(trun.Entries)), true, false))
			frag.Determined = false
		}
	}
	return frag, faults
}

// isSyncSample reads the sample_is_non_sync_sample bit (0x10000).
func isSyncSample(flags uint32) bool { return flags&0x10000 == 0 }

// CheckContinuity compares consecutive fragment timelines and reports gaps or
// overlaps. A gap is recorded on the later fragment at its moof offset.
func CheckContinuity(frags []Fragment, uri string) []fault.Fault {
	var out []fault.Fault
	var prevEnd int64
	var havePrev bool
	for _, f := range frags {
		if !f.Determined || !f.TimescaleValid() {
			havePrev = false
			continue
		}
		start := f.StartCanonical()
		if havePrev && start != prevEnd {
			out = append(out, fault.MustNew(fault.KindTimestampDiscontinuity,
				fault.Location{Segment: uri, Offset: f.MoofOffset},
				fmt.Sprintf("timeline start %d != previous end %d", start, prevEnd), true, true))
		}
		prevEnd = start + f.DurationCanonical()
		havePrev = true
	}
	return out
}
