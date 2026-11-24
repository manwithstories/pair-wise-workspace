// Package inspect validates fragmented MP4 structure with go-mp4 and derives
// AVC/HEVC key-frame and GOP statistics from sample data.
package inspect

import (
	"io"

	"github.com/abema/go-mp4"

	"mediac/internal/core"
)

// Read bounds for bitstream analysis.
const (
	MaxMediaBytes = 128 << 20 // 128 MiB per fragment
)

// SampleInfo describes one trun sample after structural validation.
type SampleInfo struct {
	Index       int    `json:"index"`
	Size        uint32 `json:"size"`
	Duration    uint32 `json:"duration"` // movie timescale ticks
	IsKeyframe bool   `json:"is_keyframe"`
	NALTypes    []byte `json:"-"`
}

// FragmentReport is the structural verdict for one fMP4 media fragment.
type FragmentReport struct {
	RepID        string       `json:"rep_id"`
	Fragment     int          `json:"fragment"`
	CodecHint    string       `json:"codec_hint,omitempty"`
	Timescale    uint32       `json:"timescale"`
	BaseDecodeT  uint64       `json:"base_decode_time"`
	Samples      []SampleInfo `json:"samples"`
	Truncated    bool         `json:"truncated"`
	HasZeroDur   bool         `json:"has_zero_duration"`
	HasInvalidNAL bool       `json:"has_invalid_nal"`
	// BoxBoundaries records declared size vs. readable bytes per moof/mdat so
	// truncation is demonstrable rather than inferred.
	BoxBoundaries []BoxBoundary `json:"box_boundaries,omitempty"`
	Faults        []core.Fault  `json:"faults,omitempty"`
}

// BoxBoundary pairs a box type with its declared and observed byte sizes.
type BoxBoundary struct {
	Type     string `json:"type"`
	Declared uint64 `json:"declared"`
	Readable uint64 `json:"readable"`
	Offset   int64  `json:"offset"`
}

// InspectFragment parses one fMP4 fragment (moof + mdat) from r. The
// representation id and fragment index are carried on every emitted fault.
func InspectFragment(repID string, fragment int, codec string, r io.ReadSeeker,
	rec *core.Recorder) (*FragmentReport, error) {
	report := &FragmentReport{RepID: repID, Fragment: fragment, CodecHint: codec}
	src := core.Source{RepID: repID, SegmentIndex: fragment}

	// Walk every top-level box, recording boundaries and collecting payloads.
	var moofPayload []byte
	var mdatPayload []byte
	handler := func(h *mp4.ReadHandle) (interface{}, error) {
		bi := h.BoxInfo
		if len(h.Path) == 1 && (bi.Type == mp4.BoxTypeMoof() || bi.Type == mp4.BoxTypeMdat()) {
			boundary := BoxBoundary{
				Type: bi.Type.String(), Declared: bi.Size,
				Readable: readableBytes(r, bi), Offset: int64(bi.Offset),
			}
			report.BoxBoundaries = append(report.BoxBoundaries, boundary)
		}
		switch bi.Type {
		case mp4.BoxTypeMdat():
			data, err := readBoxData(r, h, bi)
			if err != nil {
				return nil, markTruncated(rec, report, src, bi, err)
			}
			mdatPayload = data
			return nil, nil
		case mp4.BoxTypeMoof():
			// Expand children so trun/tfhd/tfdt are validated by go-mp4.
			vals, err := h.Expand()
			if err != nil {
				return nil, markTruncated(rec, report, src, bi, err)
			}
			if data, derr := readBoxData(r, h, bi); derr == nil {
				moofPayload = data
			}
			return vals, err
		default:
			return h.Expand()
		}
	}
	if _, err := mp4.ReadBoxStructure(r, handler); err != nil {
		if f, ok := core.AsFault(err); ok {
			rec.Add(f)
			report.Faults = append(report.Faults, f)
			return report, nil
		}
		return nil, err
	}
	if err := validateMoof(repID, fragment, moofPayload, report, rec); err != nil {
		return report, nil
	}
	if len(report.Samples) == 0 {
		f := core.NewFault(core.FaultTruncatedBox, core.SevDamaged,
			"fragment contains no trun samples", src)
		rec.Add(f)
		report.Faults = append(report.Faults, f)
		report.Truncated = true
		return report, nil
	}
	analyzeSamples(repID, fragment, codec, mdatPayload, report, rec)
	return report, nil
}

// markTruncated normalizes every short-read inside a box into the shared
// truncated-box fault instead of leaking an opaque IO error.
func markTruncated(rec *core.Recorder, rep *FragmentReport, src core.Source,
	bi mp4.BoxInfo, err error) error {
	rep.Truncated = true
	f := core.NewFault(core.FaultTruncatedBox, core.SevDamaged,
		bi.Type.String()+" box shorter than declared: "+err.Error(),
		core.Source{RepID: src.RepID, SegmentIndex: src.SegmentIndex,
			OffsetBytes: int64(bi.Offset)})
	rec.Add(f)
	rep.Faults = append(rep.Faults, f)
	return f
}

func readableBytes(r io.Seeker, bi mp4.BoxInfo) uint64 {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0
	}
	avail := end - int64(bi.Offset)
	if avail < 0 {
		return 0
	}
	if uint64(avail) < bi.Size {
		return uint64(avail)
	}
	return bi.Size
}

func readBoxData(r io.ReadSeeker, h *mp4.ReadHandle, bi mp4.BoxInfo) ([]byte, error) {
	payloadOff, err := bi.SeekToPayload(r)
	if err != nil {
		return nil, err
	}
	size := int64(bi.Size) - int64(bi.HeaderSize)
	if size > MaxMediaBytes {
		return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
			"box exceeds media size limit", core.Source{OffsetBytes: payloadOff})
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	if int64(n) < size {
		return buf[:n], io.ErrUnexpectedEOF
	}
	return buf, nil
}
