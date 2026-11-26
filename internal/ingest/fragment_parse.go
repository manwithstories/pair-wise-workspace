package ingest

import (
	"fmt"
	"io"

	"github.com/abema/go-mp4"

	"relay/internal/media"
)

// parseMoof walks the moof/traf children. All seeks use raw-relative offsets
// (the fragment starts at raw[0]); faultBias is the fragment's source-level
// offset, added only when constructing fault offsets.
func parseMoof(r io.ReadSeeker, moofH BoxHeader, faultBias int64) (
	*mp4.Mfhd, *mp4.Tfhd, *mp4.Tfdt, *mp4.Trun, error,
) {
	payloadEnd := int64(moofH.Size)
	var (
		mfhd *mp4.Mfhd
		tfhd *mp4.Tfhd
		tfdt *mp4.Tfdt
		trun *mp4.Trun
	)
	rel := int64(moofH.Header)
	for rel < payloadEnd {
		if _, err := r.Seek(rel, io.SeekStart); err != nil {
			return nil, nil, nil, nil, err
		}
		h, err := ReadBoxHeader(r, rel)
		if err != nil {
			return nil, nil, nil, nil, media.NewFault(media.FaultTruncated, 0, 0,
				faultBias+rel, "moof child header truncated")
		}
		if h.Size == 0 || rel+int64(h.Size) > payloadEnd {
			return nil, nil, nil, nil, media.NewFault(media.FaultTruncated, 0, 0,
				faultBias+rel, "moof child box runs past moof payload")
		}
		if h.Type == mp4.BoxTypeTraf() {
			if err := parseTraf(r, h, rel, faultBias, &mfhd, &tfhd, &tfdt, &trun); err != nil {
				return nil, nil, nil, nil, err
			}
		}
		rel += int64(h.Size)
	}
	if mfhd == nil || tfhd == nil || tfdt == nil || trun == nil {
		return nil, nil, nil, nil, media.NewFault(media.FaultStructure, 0, 0,
			faultBias, "moof/traf must contain mfhd, tfhd, tfdt and trun")
	}
	return mfhd, tfhd, tfdt, trun, nil
}

// parseTraf walks one traf's children. rel is the traf box's raw-relative
// offset; faultBias again converts raw offsets into source offsets.
func parseTraf(r io.ReadSeeker, trafH BoxHeader, rel, faultBias int64,
	mfhd **mp4.Mfhd, tfhd **mp4.Tfhd, tfdt **mp4.Tfdt, trun **mp4.Trun) error {
	end := rel + int64(trafH.Size)
	cur := rel + int64(trafH.Header)
	for cur < end {
		if _, err := r.Seek(cur, io.SeekStart); err != nil {
			return err
		}
		h, err := ReadBoxHeader(r, cur)
		if err != nil {
			return media.NewFault(media.FaultTruncated, 0, 0, faultBias+cur,
				"traf child header truncated")
		}
		if h.Size == 0 || cur+int64(h.Size) > end {
			return media.NewFault(media.FaultTruncated, 0, 0, faultBias+cur,
				"traf child box runs past traf payload")
		}
		if _, err := r.Seek(cur+int64(h.Header), io.SeekStart); err != nil {
			return err
		}
		payload := h.Size - h.Header
		switch h.Type {
		case mp4.BoxTypeMfhd():
			var v mp4.Mfhd
			if err := unmarshalBox(r, payload, &v); err != nil {
				return wrapUnmarshal(err, faultBias+cur, "mfhd")
			}
			*mfhd = &v
		case mp4.BoxTypeTfhd():
			var v mp4.Tfhd
			if err := unmarshalBox(r, payload, &v); err != nil {
				return wrapUnmarshal(err, faultBias+cur, "tfhd")
			}
			*tfhd = &v
		case mp4.BoxTypeTfdt():
			var v mp4.Tfdt
			if err := unmarshalBox(r, payload, &v); err != nil {
				return wrapUnmarshal(err, faultBias+cur, "tfdt")
			}
			*tfdt = &v
		case mp4.BoxTypeTrun():
			var v mp4.Trun
			if err := unmarshalBox(r, payload, &v); err != nil {
				return wrapUnmarshal(err, faultBias+cur, "trun")
			}
			*trun = &v
		}
		cur += int64(h.Size)
	}
	return nil
}

func unmarshalBox(r io.ReadSeeker, payloadSize uint64, dst mp4.IBox) error {
	_, err := mp4.Unmarshal(r, payloadSize, dst, mp4.Context{})
	return err
}

func wrapUnmarshal(err error, offset int64, box string) error {
	return media.NewFault(media.FaultTruncated, 0, 0, offset,
		fmt.Sprintf("%s parse failed: %v", box, err))
}

// buildSamples expands trun entries into Samples and validates every declared
// sample range against the mdat payload; the first out-of-range sample's
// source offset is reported.
func buildSamples(f *Fragment, trun *mp4.Trun, moofH, mdatH BoxHeader,
	faultBias, mdatPayloadLen int64) error {
	count := int(trun.SampleCount)
	if count != len(trun.Entries) {
		return media.NewFault(media.FaultTruncated, f.TrackID, f.Sequence,
			faultBias, fmt.Sprintf("trun sample_count=%d but entries=%d",
				count, len(trun.Entries)))
	}
	firstData := int64(trun.DataOffset) // relative to moof start (raw offset 0)
	if firstData < 0 || firstData >= int64(len(f.Raw)) {
		return media.NewFault(media.FaultSampleRange, f.TrackID, f.Sequence,
			faultBias+int64(moofH.Size),
			fmt.Sprintf("trun data_offset %d outside fragment", trun.DataOffset))
	}
	cursor := firstData
	samples := make([]Sample, 0, count)
	mdatStart := int64(moofH.Size) + int64(mdatH.Header)
	for i, e := range trun.Entries {
		size := int64(e.SampleSize)
		if size <= 0 {
			return media.NewFault(media.FaultSampleRange, f.TrackID, f.Sequence,
				faultBias+cursor, fmt.Sprintf("sample %d has non-positive size %d", i, size))
		}
		if cursor+size > int64(len(f.Raw)) {
			return media.NewFault(media.FaultSampleRange, f.TrackID, f.Sequence,
				faultBias+cursor, fmt.Sprintf("sample %d [%d,+%d] runs past fragment end",
					i, cursor, size))
		}
		if cursor < mdatStart || cursor+size > mdatStart+mdatPayloadLen {
			return media.NewFault(media.FaultSampleRange, f.TrackID, f.Sequence,
				faultBias+cursor, fmt.Sprintf("sample %d outside mdat payload", i))
		}
		flags := e.SampleFlags
		samples = append(samples, Sample{
			Index:             uint32(i + 1),
			Duration:          e.SampleDuration,
			Size:              e.SampleSize,
			Flags:             flags,
			CompositionOffset: e.SampleCompositionTimeOffsetV1,
			DataOffset:        faultBias + cursor,
			Sync:              flags&0x10000 == 0,
		})
		cursor += size
	}
	f.Samples = samples
	return nil
}
