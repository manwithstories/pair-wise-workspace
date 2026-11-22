package scan

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"

	"mediaslice/media"
)

// Config controls scanner behaviour.
type Config struct {
	// KeyframeWaitTicks bounds how long the cutter waits for a keyframe.
	KeyframeWaitTicks media.Ticks
}

// DefaultConfig returns the scanner defaults (one segment window).
func DefaultConfig() Config {
	return Config{KeyframeWaitTicks: media.FromSeconds(4)}
}

// ScanFile hashes, structurally walks and fully parses one MP4/MOV source,
// optionally pairing video samples with an RTP sidecar. Per-track failures
// are recorded on the index and never abort sibling tracks.
func ScanFile(path, rtpPath string, cfg Config) (*FileIndex, error) {
	sum, size, err := HashFile(path)
	if err != nil {
		return nil, err
	}
	idx := &FileIndex{
		ParserVersion: ParserVersion,
		SourcePath:    path,
		InputSHA256:   sum,
		FileSize:      size,
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, media.NewError(media.KindIO, path, err.Error())
	}
	defer f.Close()

	root, walkErrs := walkBoxes(f, size, path)
	idx.Errors = append(idx.Errors, walkErrs...)

	mdat := root.findFirst("mdat")
	if mdat != nil {
		idx.MdatStart = int64(mdat.info.Offset + mdat.info.HeaderSize)
		idx.MdatEnd = int64(mdat.info.Offset + mdat.info.Size)
	} else {
		idx.Errors = append(idx.Errors, media.NewError(media.KindMissingTrack, path,
			"no mdat box present"))
	}

	moov := root.findFirst("moov")
	if moov == nil {
		idx.Errors = append(idx.Errors, media.NewError(media.KindMissingTrack, path,
			"no moov box present"))
		return idx, nil
	}
	traks := moov.findAll("trak")
	if len(traks) == 0 {
		idx.Errors = append(idx.Errors, media.NewError(media.KindMissingTrack, path,
			"moov contains no trak boxes"))
	}
	for _, tnode := range traks {
		ti, terr := parseTrack(f, path, tnode, idx)
		if terr != nil {
			ti.Stopped = true
			ti.Errors = append(ti.Errors, terr)
			idx.Errors = append(idx.Errors, terr)
		}
		idx.Tracks = append(idx.Tracks, *ti)
		idx.Summaries = append(idx.Summaries, summarize(ti))
	}

	// RTP sidecar is attached to the first video track.
	if rtpPath != "" {
		if err := attachRTP(rtpPath, idx); err != nil {
			idx.Errors = append(idx.Errors, err)
		}
	}
	return idx, nil
}

// parseTrack extracts handler/timescale/edits and the full sample table.
func parseTrack(f *os.File, source string, node *boxNode, idx *FileIndex) (*TrackIndex, *media.MediaError) {
	ti := &TrackIndex{}
	tkhdNode := node.findFirst("tkhd")
	if tkhdNode == nil {
		return ti, media.NewError(media.KindMissingTrack, source, "tkhd missing").
			WithOffset(int64(node.info.Offset), 8)
	}
	var tkhd tkhdBox
	if err := payloadAt(f, tkhdNode, &tkhd); err != nil {
		return ti, media.NewError(media.KindBadSampleTable, source, err.Error()).
			WithOffset(int64(tkhdNode.info.Offset), 8)
	}
	ti.TrackID = tkhd.TrackID

	mdhdNode := node.findFirst("mdhd")
	if mdhdNode == nil {
		return ti, media.NewError(media.KindMissingTrack, source, "mdhd missing").
			WithTrack(ti.TrackID).WithOffset(int64(node.info.Offset), 8)
	}
	var mdhd mdhdBox
	if err := payloadAt(f, mdhdNode, &mdhd); err != nil {
		return ti, media.NewError(media.KindBadSampleTable, source, err.Error()).
			WithTrack(ti.TrackID).WithOffset(int64(mdhdNode.info.Offset), 8)
	}
	ti.Timescale = mdhd.Timescale
	if ti.Timescale == 0 {
		return ti, media.NewError(media.KindBadSampleTable, source, "zero timescale").
			WithTrack(ti.TrackID).WithOffset(int64(mdhdNode.info.Offset), 4)
	}
	if h := node.findFirst("hdlr"); h != nil {
		var hdlr hdlrBox
		if err := payloadAt(f, h, &hdlr); err == nil {
			ti.Handler = hdlrHandlerType(&hdlr)
		}
	}

	if elstNode := node.findFirst("elst"); elstNode != nil {
		edits, eerr := parseEdits(f, source, elstNode, ti.TrackID)
		if eerr != nil {
			return ti, eerr
		}
		ti.Edits = edits
	}

	merr := parseSamples(f, source, node, ti, idx)
	if merr != nil {
		return ti, merr
	}
	return ti, nil
}

// parseEdits validates and retains every edit list entry. Negative media
// times (delay edits) are skipped from mapping but recorded; empty edits
// produce a diagnosable error.
func parseEdits(f *os.File, source string, node *boxNode, trackID uint32) ([]EditRecord, *media.MediaError) {
	var elst elstBox
	if err := payloadAt(f, node, &elst); err != nil {
		return nil, media.NewError(media.KindEditList, source, err.Error()).
			WithTrack(trackID).WithOffset(int64(node.info.Offset), 8)
	}
	if int(elst.EntryCount) != len(elst.Entries) {
		return nil, media.NewError(media.KindBadSampleTable, source,
			fmt.Sprintf("elst entry_count=%d but %d entries decoded", elst.EntryCount, len(elst.Entries))).
			WithTrack(trackID).WithOffset(int64(node.info.Offset), 4)
	}
	out := make([]EditRecord, 0, len(elst.Entries))
	for _, e := range elst.Entries {
		dur, mt, rate := elstValues(&e)
		if dur == 0 {
			return nil, media.NewError(media.KindEditList, source, "empty (zero-duration) edit segment").
				WithTrack(trackID).WithOffset(int64(node.info.Offset), 8)
		}
		if mt < 0 {
			// Negative media time is an edit-list delay; retain it for
			// diagnostics but it must not map any samples.
			out = append(out, EditRecord{SegmentDuration: dur, MediaTime: mt, Rate: rate})
			continue
		}
		out = append(out, EditRecord{SegmentDuration: dur, MediaTime: mt, Rate: rate})
	}
	return out, nil
}

// summarize builds the compact per-track digest stored in the index header.
func summarize(ti *TrackIndex) TrackSummary {
	h := sha256.New()
	var buf [8]byte
	for _, s := range ti.Samples {
		binary.LittleEndian.PutUint64(buf[:], uint64(s.Offset))
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(s.Size))
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(s.StartTick))
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(s.Duration))
		h.Write(buf[:])
	}
	return TrackSummary{
		TrackID:      ti.TrackID,
		Handler:      ti.Handler,
		SampleCount:  len(ti.Samples),
		SampleDigest: fmt.Sprintf("%x", h.Sum(nil)),
	}
}
