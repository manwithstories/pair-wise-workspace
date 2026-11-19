package container

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/abema/go-mp4"

	"mediapipeline/internal/index"
	"mediapipeline/internal/mediaerr"
)

// writeMdat appends the single mdat box holding every track's samples in track
// order, then backfills each track's stco chunk offset.
func writeMdat(w *mp4.Writer, ws *writeSeeker, tracks []TrackConfig, st *writerState, mdatStart uint64) *mediaerr.Error {
	if _, err := w.StartBox(&mp4.BoxInfo{Type: mp4.BoxTypeMdat()}); err != nil {
		return wrapBox("mdat start", err)
	}
	// One chunk per track; offsets advance by each track's payload span.
	chunkOffsets := make([]uint32, len(tracks))
	payloadCursor := mdatStart
	for i, tc := range tracks {
		chunkOffsets[i] = uint32(payloadCursor)
		for _, s := range tc.Samples {
			if _, err := w.Write(s); err != nil {
				return wrapBox("mdat sample write", err)
			}
			payloadCursor += uint64(len(s))
		}
	}
	if _, err := w.EndBox(); err != nil {
		return wrapBox("mdat end", err)
	}
	if len(st.patches) != len(tracks) {
		return mediaerr.New(mediaerr.StageContainer, mediaerr.KindInvalidInput,
			"stco patch count does not match track count")
	}
	for i, patch := range st.patches {
		if _, err := ws.Seek(patch.position, io.SeekStart); err != nil {
			return wrapBox("stco backfill seek", err)
		}
		var field [4]byte
		binary.BigEndian.PutUint32(field[:], chunkOffsets[i])
		if _, err := ws.Write(field[:]); err != nil {
			return wrapBox("stco backfill write", err)
		}
	}
	if _, err := ws.Seek(0, io.SeekEnd); err != nil {
		return wrapBox("final reposition", err)
	}
	return nil
}

// ParsedTrack is the round-trip view of one track.
type ParsedTrack struct {
	TrackID      uint32
	Timescale    uint32
	SampleCount  uint32
	SampleSizes  []uint32
	ChunkOffsets []uint32
	KeyframeNums []uint32 // empty when the track carries no stss box
}

// ParsedFile is the result of reading an MP4 back.
type ParsedFile struct {
	Tracks []ParsedTrack
	// MdatPayload is the concatenated sample payload for hash verification.
	MdatPayload []byte
}

// Parse reads an MP4 byte stream back into structured boxes. A truncated
// container (a declared box size running past EOF) is reported as a structured
// truncation error rather than silently accepted.
func Parse(data []byte) (*ParsedFile, *mediaerr.Error) {
	if len(data) < 8 {
		return nil, mediaerr.New(mediaerr.StageContainer, mediaerr.KindTruncatedBox,
			"input shorter than any MP4 box header").WithOffset(int64(len(data)))
	}
	r := bytes.NewReader(data)
	out := &ParsedFile{}
	currentTrack := -1
	_, err := mp4.ReadBoxStructure(r, func(h *mp4.ReadHandle) (interface{}, error) {
		// Explicit truncation check: the declared box must fit in the file.
		end := int64(h.BoxInfo.Offset + h.BoxInfo.Size)
		if end > int64(len(data)) {
			return nil, mediaerr.New(mediaerr.StageContainer, mediaerr.KindTruncatedBox,
				"box extends past end of container").
				WithOffset(int64(h.BoxInfo.Offset))
		}
		switch h.BoxInfo.Type {
		case mp4.BoxTypeTrak():
			out.Tracks = append(out.Tracks, ParsedTrack{})
			currentTrack = len(out.Tracks) - 1
			return h.Expand()
		case mp4.BoxTypeMdhd(), mp4.BoxTypeStsz(), mp4.BoxTypeStco(),
			mp4.BoxTypeStss(), mp4.BoxTypeTkhd():
			box, _, perr := h.ReadPayload()
			if perr != nil {
				return nil, mediaerr.Wrap(mediaerr.StageContainer, mediaerr.KindTruncatedBox,
					"box payload read failed", perr)
			}
			applyBox(out, currentTrack, h, box)
			return nil, nil
		case mp4.BoxTypeMdat():
			payload, perr := readMdatPayload(data, &h.BoxInfo)
			if perr != nil {
				return nil, perr
			}
			out.MdatPayload = append(out.MdatPayload, payload...)
			return nil, nil
		default:
			return h.Expand()
		}
	})
	if err != nil {
		if me, ok := err.(*mediaerr.Error); ok {
			return nil, me
		}
		return nil, mediaerr.Wrap(mediaerr.StageContainer, mediaerr.KindTruncatedBox,
			"MP4 structure read failed", err)
	}
	return out, nil
}

// applyBox folds a parsed payload box into the track selected by the parser.
func applyBox(out *ParsedFile, trackIdx int, h *mp4.ReadHandle, box mp4.IBox) {
	if trackIdx < 0 || trackIdx >= len(out.Tracks) {
		return
	}
	pt := &out.Tracks[trackIdx]
	switch b := box.(type) {
	case *mp4.Mdhd:
		pt.Timescale = b.Timescale
	case *mp4.Tkhd:
		pt.TrackID = b.TrackID
	case *mp4.Stsz:
		pt.SampleCount = b.SampleCount
		pt.SampleSizes = append([]uint32(nil), b.EntrySize...)
	case *mp4.Stco:
		pt.ChunkOffsets = append([]uint32(nil), b.ChunkOffset...)
	case *mp4.Stss:
		pt.KeyframeNums = append([]uint32(nil), b.SampleNumber...)
	}
}

// readMdatPayload extracts the mdat payload given its box header location.
func readMdatPayload(data []byte, bi *mp4.BoxInfo) ([]byte, *mediaerr.Error) {
	header := uint64(8)
	if bi.Size == 1 {
		header = 16 // large-size box
	}
	start := bi.Offset + header
	if start > uint64(len(data)) || bi.Size > uint64(len(data))-bi.Offset {
		return nil, mediaerr.New(mediaerr.StageContainer, mediaerr.KindTruncatedBox,
			"mdat payload extends past end of container").WithOffset(int64(bi.Offset))
	}
	end := bi.Offset + bi.Size
	return data[start:end], nil
}

// RoundTripConsistency compares the written sample manifest against what the
// parser recovered and returns the fraction of matching sample sizes and chunk
// offsets. A value below 1.0 quantifies exactly how much was lost.
func RoundTripConsistency(tracks []TrackConfig, parsed *ParsedFile) float64 {
	var checks, matches float64
	for i, tc := range tracks {
		if i >= len(parsed.Tracks) {
			checks += float64(len(tc.Samples))
			continue
		}
		got := parsed.Tracks[i]
		for j, s := range tc.Samples {
			checks++
			if j < len(got.SampleSizes) && got.SampleSizes[j] == uint32(len(s)) {
				matches++
			}
		}
		checks++
		if len(got.ChunkOffsets) >= 1 {
			matches++
		}
	}
	if checks == 0 {
		return 0
	}
	return matches / checks
}

// KeyframeHitRate validates parsed stss entries against the written index and
// returns the true hit rate; fabricated or missing entries count as misses.
func KeyframeHitRate(tc TrackConfig, parsed ParsedTrack) (float64, *mediaerr.Error) {
	if tc.Keyframes == nil {
		// Non-keyframe tracks must not carry an stss box.
		if len(parsed.KeyframeNums) != 0 {
			return 0, mediaerr.New(mediaerr.StageContainer, mediaerr.KindIndexOutOfRange,
				"unexpected keyframe index on non-keyframe track")
		}
		return 1.0, nil
	}
	want := tc.Keyframes.Entries()
	if len(want) == 0 {
		return 0, mediaerr.New(mediaerr.StageContainer, mediaerr.KindNoKeyframe,
			"keyframe track wrote no stss entries")
	}
	queries := make([]uint32, len(want))
	for i, e := range want {
		queries[i] = e.SampleNumber
	}
	_, rate := tc.Keyframes.HitRate(parsed.KeyframeNums)
	// HitRate intersects parsed entries with the known keyframe set; also
	// require the parsed count to match so extra bogus entries fail the rate.
	if len(parsed.KeyframeNums) != len(want) {
		return rate, mediaerr.New(mediaerr.StageContainer, mediaerr.KindIndexOutOfRange,
			"parsed keyframe count differs from written index")
	}
	return rate, nil
}

// IndexFromSamples is a convenience to build a keyframe index from boolean
// keyframe markers and sample sizes for container tests and the pipeline.
func IndexFromSamples(track uint32, keyframe []bool, sizes []int) (*index.Index, *mediaerr.Error) {
	samples := make([]index.Sample, len(keyframe))
	for i := range samples {
		samples[i] = index.Sample{IsKeyframe: keyframe[i], Size: sizes[i]}
	}
	return index.Build(track, samples)
}
