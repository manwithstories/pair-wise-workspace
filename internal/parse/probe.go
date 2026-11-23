package parse

import (
	"io"

	"github.com/abema/go-mp4"

	"archivecheck/internal/fault"
)

// Probe describes a media segment's ordering metadata without validating its
// samples. It is cheap enough to use for sequencing before streaming work.
type Probe struct {
	Group      int
	Segment    string
	HasMfhd    bool
	Sequence   uint32
	MoofOffset int64
}

// ProbeReader reads only the moof box to extract mfhd ordering metadata. It
// still enforces box bounds, so a truncated header is reported with its offset.
func ProbeReader(r io.ReadSeeker, segment string) (Probe, error) {
	pos, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return Probe{}, fault.Wrap(err, fault.KindShortRead, segment, "", 0, "cannot determine size")
	}
	return ProbeSegment(r, pos, segment)
}

// ProbeSegment probes a reader of known size.
func ProbeSegment(r io.ReadSeeker, size int64, segment string) (Probe, error) {
	p := Probe{Segment: segment, MoofOffset: -1}
	var moofPayload []byte
	var moof Header
	seen := false
	err := WalkReader(r, size, segment, func(h Header, rr io.ReadSeeker) error {
		if h.Type != "moof" {
			return nil
		}
		if seen {
			return fault.New(fault.KindMalformed, segment, "moof", h.Offset, "multiple moof boxes")
		}
		seen = true
		moof = h
		payload, err := ReadPayload(rr, h, segment)
		if err != nil {
			return err
		}
		moofPayload = payload
		return nil
	})
	if err != nil {
		return p, err
	}
	if !seen {
		return p, fault.New(fault.KindMissingBox, segment, "moof", 0, "media segment has no moof")
	}
	p.MoofOffset = moof.Offset
	if err := WalkPayload(moofPayload, moof.PayloadOffset(), segment, func(h Header, payload []byte) error {
		if h.Type != "mfhd" {
			return nil
		}
		var mfhd mp4.Mfhd
		if err := unmarshalChild(payload, h.PayloadOffset(), segment, "mfhd", &mfhd); err != nil {
			return err
		}
		if p.HasMfhd {
			return fault.New(fault.KindMalformed, segment, "mfhd", h.Offset, "multiple mfhd boxes")
		}
		p.HasMfhd = true
		p.Sequence = mfhd.SequenceNumber
		return nil
	}); err != nil {
		return p, err
	}
	return p, nil
}
