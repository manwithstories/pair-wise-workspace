// Package media reads and writes the fragmented-MP4 segments used by
// the pipeline. Writing exists so fixtures ship with the code; reading
// (the sample index) feeds the cutter, silence and fingerprint stages.
package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	mp4 "github.com/abema/go-mp4"

	"mediacore/internal/clock"
)

// Sample is one entry of a segment's sample index.
type Sample struct {
	Duration uint32 // in track timescale units
	Size     uint32 // bytes inside mdat
	Keyframe bool
}

// Segment is the decoded index of one fMP4 media segment, plus its
// payload when requested.
type Segment struct {
	DecodeTime uint64 // tfdt base media decode time
	Samples    []Sample
	Payload    []byte
}

// TotalSize sums the sample sizes.
func (s *Segment) TotalSize() int {
	n := 0
	for _, sm := range s.Samples {
		n += int(sm.Size)
	}
	return n
}

// TotalDuration sums the sample durations.
func (s *Segment) TotalDuration() uint64 {
	var n uint64
	for _, sm := range s.Samples {
		n += uint64(sm.Duration)
	}
	return n
}

const nonSyncFlag = 0x00010000

// WriteSegment serializes one media segment: styp, moof(mfhd,
// traf(tfhd, tfdt, trun)), mdat. The payload must match the summed
// sample sizes.
func WriteSegment(w io.Writer, decodeTime uint64, samples []Sample, payload []byte) error {
	var total int
	for _, s := range samples {
		total += int(s.Size)
	}
	if total != len(payload) {
		return fmt.Errorf("media: payload %d bytes, samples describe %d", len(payload), total)
	}
	styp, err := marshalBox("styp", &mp4.Styp{
		MajorBrand:       [4]byte{'m', 's', 'd', 'h'},
		CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: [4]byte{'m', 's', 'd', 'h'}}},
	})
	if err != nil {
		return err
	}
	if _, err := w.Write(styp); err != nil {
		return err
	}

	mfhd := &mp4.Mfhd{}
	mfhd.SetVersion(0)
	mfhd.SequenceNumber = 1
	mfhdBox, err := marshalBox("mfhd", mfhd)
	if err != nil {
		return err
	}
	tfhd := &mp4.Tfhd{TrackID: 1}
	tfhd.SetFlags(0x020000) // default-base-is-moof
	tfhdBox, err := marshalBox("tfhd", tfhd)
	if err != nil {
		return err
	}
	tfdt := &mp4.Tfdt{}
	tfdt.SetVersion(1)
	tfdt.BaseMediaDecodeTimeV1 = decodeTime
	tfdtBox, err := marshalBox("tfdt", tfdt)
	if err != nil {
		return err
	}
	trun := &mp4.Trun{SampleCount: uint32(len(samples))}
	trun.SetVersion(0)
	trun.SetFlags(0x000100 | 0x000200 | 0x000400) // duration|size|flags
	for _, s := range samples {
		var flags uint32
		if !s.Keyframe {
			flags = nonSyncFlag
		}
		trun.Entries = append(trun.Entries, mp4.TrunEntry{
			SampleDuration: s.Duration,
			SampleSize:     s.Size,
			SampleFlags:    flags,
		})
	}
	trunBox, err := marshalBox("trun", trun)
	if err != nil {
		return err
	}
	var traf bytes.Buffer
	traf.Write(tfhdBox)
	traf.Write(tfdtBox)
	traf.Write(trunBox)
	var moof bytes.Buffer
	moof.Write(mfhdBox)
	writeBoxHeader(&moof, "traf", traf.Len())
	moof.Write(traf.Bytes())
	writeBoxHeader(w, "moof", moof.Len())
	if _, err := w.Write(moof.Bytes()); err != nil {
		return err
	}
	mdat, err := marshalBox("mdat", &mp4.Mdat{Data: payload})
	if err != nil {
		return err
	}
	_, err = w.Write(mdat)
	return err
}

// marshalBox serializes one box including its 8-byte header. go-mp4's
// Marshal emits only the payload, so the header is added here.
func marshalBox(typ string, box mp4.IImmutableBox) ([]byte, error) {
	var payload bytes.Buffer
	if _, err := mp4.Marshal(&payload, box, mp4.Context{}); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	writeBoxHeader(&out, typ, payload.Len())
	out.Write(payload.Bytes())
	return out.Bytes(), nil
}

func writeBoxHeader(w io.Writer, typ string, payloadLen int) {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(payloadLen+8))
	copy(hdr[4:], typ)
	_, _ = w.Write(hdr[:])
}

// ReadSegment decodes the sample index of one segment. When withPayload
// is false the mdat contents are skipped (bounded memory). A segment
// whose mdat is shorter than the declared sample sizes is reported as
// truncated.
func ReadSegment(r io.ReadSeeker, trackID string, segIndex int, withPayload bool) (*Segment, error) {
	const op = "media-probe"
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, clock.Wrap(clock.CodeIO, op, err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, clock.Wrap(clock.CodeIO, op, err)
	}
	seg := &Segment{}
	var mdatLen uint64
	var payload bytes.Buffer
	_, err = mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (any, error) {
		switch h.BoxInfo.Type {
		case mp4.BoxTypeMoof(), mp4.BoxTypeTraf():
			_, err := h.Expand()
			return nil, err
		case mp4.BoxTypeTfdt():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			seg.DecodeTime = box.(*mp4.Tfdt).GetBaseMediaDecodeTime()
		case mp4.BoxTypeTrun():
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trun := box.(*mp4.Trun)
			if int(trun.SampleCount) != len(trun.Entries) {
				return nil, clock.At(clock.CodeTruncatedSegment, op, trackID, segIndex,
					"trun declares %d samples, carries %d", trun.SampleCount, len(trun.Entries))
			}
			for _, e := range trun.Entries {
				seg.Samples = append(seg.Samples, Sample{
					Duration: e.SampleDuration,
					Size:     e.SampleSize,
					Keyframe: e.SampleFlags&nonSyncFlag == 0,
				})
			}
		case mp4.BoxTypeMdat():
			if end := h.BoxInfo.Offset + h.BoxInfo.Size; end > uint64(fileSize) {
				return nil, clock.At(clock.CodeTruncatedSegment, op, trackID, segIndex,
					"mdat ends at %d, file is %d bytes", end, fileSize)
			}
			mdatLen = h.BoxInfo.Size - h.BoxInfo.HeaderSize
			if withPayload {
				if _, err := h.ReadData(&payload); err != nil {
					return nil, err
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		if _, ok := err.(*clock.Error); ok {
			return nil, err
		}
		return nil, clock.At(clock.CodeTruncatedSegment, op, trackID, segIndex,
			"unreadable segment: %v", err)
	}
	if len(seg.Samples) == 0 {
		return nil, clock.At(clock.CodeTruncatedSegment, op, trackID, segIndex,
			"no sample index found")
	}
	if want := seg.TotalSize(); uint64(want) != mdatLen {
		return nil, clock.At(clock.CodeTruncatedSegment, op, trackID, segIndex,
			"mdat holds %d bytes, samples describe %d", mdatLen, want)
	}
	if withPayload {
		seg.Payload = payload.Bytes()
	}
	return seg, nil
}
