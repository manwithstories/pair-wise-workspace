package inspect

import (
	"bytes"

	"github.com/abema/go-mp4"

	"mediac/internal/core"
)

// extractTrunShape records the trun-declared sample count and per-sample
// durations/sizes from moof bytes. It runs during the structural walk before
// mdat is read, so a truncated media box cannot blank the expected sample
// count. It deliberately does not emit faults; validateMoof owns that.
func extractTrunShape(moof []byte, report *FragmentReport) {
	r := bytes.NewReader(moof)
	var trun *mp4.Trun
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == mp4.BoxTypeTrun() {
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, nil
			}
			trun, _ = box.(*mp4.Trun)
			return nil, nil
		}
		return h.Expand()
	}
	if _, err := mp4.ReadBoxStructure(r, handler); err != nil || trun == nil {
		return
	}
	report.ExpectedSamples = int(trun.SampleCount)
	if cap(report.Samples) == 0 {
		report.Samples = make([]SampleInfo, 0, trun.SampleCount)
	}
}

// validateMoof re-parses the captured moof bytes to read the movie time base,
// fragment decode time and per-sample duration/size table. A second parse on
// bounded bytes keeps the structural walk independent from sample extraction.
func validateMoof(repID string, fragment int, moof []byte,
	report *FragmentReport, rec *core.Recorder) error {
	src := core.Source{RepID: repID, SegmentIndex: fragment}
	if len(moof) == 0 {
		f := core.NewFault(core.FaultTruncatedBox, core.SevDamaged,
			"missing moof box", src)
		rec.Add(f)
		report.Faults = append(report.Faults, f)
		report.Truncated = true
		return f
	}
	r := bytes.NewReader(moof)
	var trun *mp4.Trun
	var tfdt *mp4.Tfdt
	var mdhd *mp4.Mdhd
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeTrun():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun, _ = box.(*mp4.Trun)
			return nil, nil
		case mp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tfdt, _ = box.(*mp4.Tfdt)
			return nil, nil
		case mp4.BoxTypeMdhd():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			mdhd, _ = box.(*mp4.Mdhd)
			return nil, nil
		default:
			return h.Expand()
		}
	}
	if _, err := mp4.ReadBoxStructure(r, handler); err != nil {
		f := core.NewFault(core.FaultTruncatedBox, core.SevDamaged,
			"unreadable moof children: "+err.Error(), src)
		rec.Add(f)
		report.Faults = append(report.Faults, f)
		report.Truncated = true
		return f
	}
	if mdhd != nil && mdhd.Timescale > 0 {
		report.Timescale = mdhd.Timescale
	} else {
		report.Timescale = 1000 // fragments alone lack mdhd; default to the ms base
	}
	if tfdt != nil {
		report.BaseDecodeT = tfdt.GetBaseMediaDecodeTime()
	}
	if trun == nil {
		f := core.NewFault(core.FaultTruncatedBox, core.SevDamaged,
			"moof has no trun box", src)
		rec.Add(f)
		report.Faults = append(report.Faults, f)
		report.Truncated = true
		return f
	}
	report.ExpectedSamples = int(trun.SampleCount)
	report.Samples = make([]SampleInfo, 0, trun.SampleCount)
	for i, e := range trun.Entries {
		s := SampleInfo{
			Index: i, Size: e.SampleSize,
			Duration: e.SampleDuration,
		}
		if e.SampleDuration == 0 {
			report.HasZeroDur = true
			f := core.NewFault(core.FaultZeroDuration, core.SevDamaged,
				"zero-duration sample",
				core.Source{RepID: repID, SegmentIndex: fragment, SampleIndex: i})
			rec.Add(f)
			report.Faults = append(report.Faults, f)
		}
		report.Samples = append(report.Samples, s)
	}
	return nil
}
