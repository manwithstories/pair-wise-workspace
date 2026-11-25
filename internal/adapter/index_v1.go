package adapter

import (
	"bytes"
	"crypto/sha256"
	"io"

	"avpipe/internal/media"
)

// parseIndex reconciles frames against the MP4/index section. When the index
// is absent the PTS timeline is derived from continuous stepping and every
// frame is marked PTSConfidence=derived.
func parseIndex(r io.ReadSeeker, h v1Header, size int64, res *ParseResult) {
	hasIndex := h.flags&FlagHasIndex != 0 && h.indexOffset > 0
	if !hasIndex {
		fallbackPTS(res)
		res.UsedFallback = true
		res.Errors = append(res.Errors, media.NewError("parser", media.KindMissingIndex,
			int64(h.indexOffset), media.ActionFallbackIndex,
			"index section missing; deriving PTS from continuous stepping"))
		return
	}
	count := h.recordCount
	end := h.audioOffset
	if end == 0 {
		end = uint64(size) - FooterSize
	}
	if end < h.indexOffset {
		res.Errors = append(res.Errors, media.NewError("parser", media.KindOutOfBounds,
			int64(h.indexOffset), media.ActionFallbackIndex, "index bounds invalid; falling back"))
		fallbackPTS(res)
		res.UsedFallback = true
		return
	}
	entries := make([]IndexEntry, 0, count)
	buf := make([]byte, IndexEntrySize)
	off := int64(h.indexOffset)
	for i := uint32(0); i < count; i++ {
		if err := readFull(r, off, buf, size); err != nil {
			res.Errors = append(res.Errors, media.NewError("parser", media.KindTruncated, off,
				media.ActionFallbackIndex, "index truncated at row %d: %v", i, err))
			fallbackPTS(res)
			res.UsedFallback = true
			return
		}
		entries = append(entries, parseIndexEntry(buf))
		off += IndexEntrySize
	}
	res.Index = entries
	reconcileIndex(res, entries)
}

// reconcileIndex applies authoritative index PTS where they still form a
// monotone timeline; any inconsistency degrades the affected frames to
// derived confidence rather than silently rewriting the timeline.
func reconcileIndex(res *ParseResult, entries []IndexEntry) {
	bySeq := make(map[uint32]IndexEntry, len(entries))
	for _, e := range entries {
		bySeq[e.Sequence] = e
	}
	var prevPTS uint64
	havePrev := false
	for i := range res.Frames {
		f := &res.Frames[i]
		entry, ok := bySeq[f.Sequence]
		if !ok {
			f.PTSConfidence = media.ConfidenceDerived
			continue
		}
		if havePrev && entry.PTS90k <= prevPTS {
			f.Corrupt = true
			f.GapReason = media.KindPTSRegression
			f.PTSConfidence = media.ConfidenceDerived
			res.Errors = append(res.Errors, media.NewSeqError("parser", media.KindPTSRegression,
				f.Sequence, media.ActionGap, "index pts %d regresses", entry.PTS90k))
			continue
		}
		if !f.Corrupt {
			f.PTS90k = entry.PTS90k
			prevPTS = entry.PTS90k
			havePrev = true
		}
	}
}

// fallbackPTS derives a 30 fps continuous PTS timeline from sequence numbers.
func fallbackPTS(res *ParseResult) {
	step, rem := media.StepForFPS(30)
	for i := range res.Frames {
		f := &res.Frames[i]
		f.PTS90k = uint64(f.Sequence)*step + uint64(f.Sequence)*uint64(rem)/30
		f.PTSConfidence = media.ConfidenceDerived
	}
}

// hashAllPayloads hashes every payload byte in sequence order, including
// payloads of checksum-bad records, matching the generator's footer hash.
func hashAllPayloads(frames []media.Frame) [sha256.Size]byte {
	h := sha256.New()
	for _, f := range frames {
		h.Write(f.Payload)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// parseFooter verifies the trailing payload hash. A bad footer means the
// index cannot be trusted either, so the timeline is downgraded to derived.
func parseFooter(r io.ReadSeeker, h v1Header, size int64, res *ParseResult) {
	off := size - FooterSize
	var f [FooterSize]byte
	if err := readFull(r, off, f[:], size); err != nil {
		res.Errors = append(res.Errors, media.NewError("parser", media.KindTruncated, off,
			media.ActionTruncate, "footer unreadable: %v", err))
		return
	}
	var magic [4]byte
	copy(magic[:], f[0:4])
	if magic != MagicFooter {
		res.Errors = append(res.Errors, media.NewError("parser", "bad_footer_magic", off,
			media.ActionFallbackIndex, "footer marker invalid; index distrusted"))
		res.UsedFallback = true
		fallbackPTS(res)
		return
	}
	want := f[12:44]
	got := hashAllPayloads(res.Frames)
	if !bytes.Equal(got[:], want) {
		res.Errors = append(res.Errors, media.NewError("parser", media.KindChecksum, off,
			media.ActionGap, "footer payload hash mismatch; per-record checksums remain authoritative"))
	}
}
