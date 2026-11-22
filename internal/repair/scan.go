package repair

import (
	"errors"
	"fmt"
	"io"
	"os"

	"mediacheck/internal/clock"
	"mediacheck/internal/fault"
	"mediacheck/internal/probe"
)

// scanTrack recovers one track from its declared sample timeline.
//
// The walk is a single forward pass. A sample is accepted when its byte range
// lies inside the file and inside an mdat payload, and (when uniform-fill
// detection is on) its payload is not a single repeated byte. On the first
// rejection the scan stops, the damage is recorded, and scanning resumes at
// the next keyframe whose payload validates. Recovered rows are emitted in
// strictly non-decreasing DTS order; a row that would regress is treated as
// damage rather than reordered, so the original timeline is never rewritten.
func scanTrack(path string, tr *probe.Track, cfg Config) *TrackResult {
	res := &TrackResult{
		TrackID:         tr.ID,
		Kind:            tr.Kind,
		Codec:           tr.Codec,
		Timescale:       tr.Timescale,
		StartMS:         tr.StartMS,
		DurationMS:      tr.DurationMS,
		FirstUnreadable: -1,
	}
	if tr.Timescale == 0 {
		res.Status = StatusUnrecoverable
		res.addIssue(fault.ForTrack(fault.CodeTimestamp, fault.SeverityFatal, "repair", tr.ID, -1,
			"zero timescale"), cfg.MaxIssues)
		return res
	}

	f, err := os.Open(path)
	if err != nil {
		res.Status = StatusUnrecoverable
		res.addIssue(fault.Wrap(fault.CodeIO, fault.SeverityFatal, "repair", err), cfg.MaxIssues)
		return res
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		res.Status = StatusUnrecoverable
		res.addIssue(fault.Wrap(fault.CodeIO, fault.SeverityFatal, "repair", err), cfg.MaxIssues)
		return res
	}
	readMax := int(cfg.CacheBytes)
	if readMax > 1<<20 {
		readMax = 1 << 20
	}
	reader := newReader(f, st.Size(), readMax)

	cur := tr.Cursor()
	segIndex := -1
	var curSeg *Segment
	var lastAcceptedMS int64 = -1
	var lastSeenMS int64 = -1
	var lastKeyMS int64 = -1
	var dam *Damage // non-nil while scanning skips damaged samples

	// resumeSpan closes an unrecoverable window once a verifiable sync
	// boundary is reached again.
	resumeSpan := func(resumeMS int64) {
		if dam == nil {
			return
		}
		end := resumeMS
		if end < dam.StartMS {
			end = dam.StartMS
		}
		res.Unrecoverable = append(res.Unrecoverable, Span{
			StartMS: dam.StartMS,
			EndMS:   end,
		})
		dam.ResumeMS = resumeMS
		res.Damages = append(res.Damages, *dam)
		dam = nil
	}

	for {
		row, ok := cur.Next()
		if !ok {
			break
		}
		regressed := lastSeenMS >= 0 && row.DTSMS < lastSeenMS
		if !regressed {
			lastSeenMS = row.DTSMS
		}

		if dam != nil {
			// Skipping: accept only a verifiable sync boundary that is not a
			// timestamp regression. Audio frames are all sync, so audio
			// resumes on the next intact frame.
			if regressed || !row.Keyframe {
				exclude(res, row)
				continue
			}
			if reason, _ := validateRow(reader, row, tr, cfg); reason != "" {
				exclude(res, row)
				continue
			}
			if lastAcceptedMS >= 0 && row.DTSMS < lastAcceptedMS {
				exclude(res, row)
				continue
			}
			resumeSpan(row.DTSMS)
		} else {
			reason, code := "", fault.Code("")
			if regressed {
				reason, code = "decode timestamp regression", fault.CodeTimestamp
			} else {
				reason, code = validateRow(reader, row, tr, cfg)
			}
			if reason != "" {
				dam = &Damage{
					TrackID:     tr.ID,
					Code:        code,
					Offset:      row.Offset,
					SampleIndex: row.Index,
					StartMS:     row.DTSMS,
					ResumeMS:    -1,
					Detail:      reason,
				}
				if res.FirstUnreadable < 0 || (row.InBounds && row.Offset < res.FirstUnreadable) {
					res.FirstUnreadable = row.Offset
				}
				exclude(res, row)
				curSeg = nil
				continue
			}
		}

		// Accept the row.
		lastAcceptedMS = row.DTSMS
		if row.Segment != segIndex {
			segIndex = row.Segment
			res.Segments = append(res.Segments, Segment{
				Index:      segIndex,
				StartMS:    clock.SegmentStart(segIndex, cfg.SegmentMS),
				FirstDTSMS: row.DTSMS,
				LastDTSMS:  row.DTSMS,
				FirstCTSMS: row.CTSMS,
				LastCTSMS:  row.CTSMS,
			})
		}
		if len(res.Segments) > 0 {
			curSeg = &res.Segments[len(res.Segments)-1]
		}
		if curSeg != nil && curSeg.Index == row.Segment {
			curSeg.Samples++
			curSeg.Bytes += int64(row.Size)
			curSeg.LastDTSMS = row.DTSMS
			curSeg.LastCTSMS = row.CTSMS
			if row.Keyframe {
				curSeg.Keyframes++
			}
			dur := row.EndDTSMS - row.DTSMS
			if dur <= 0 {
				dur = 1
			}
			curSeg.DurationMS += dur
		}
		if row.Keyframe {
			res.KeyframeGaps = appendGap(res.KeyframeGaps, lastKeyMS, row.DTSMS)
			lastKeyMS = row.DTSMS
			res.Recovered.Keyframes++
			if len(res.KeyframeTimes) < maxKeyframeTimes {
				res.KeyframeTimes = append(res.KeyframeTimes, row.DTSMS)
				res.KeyframeSamples = append(res.KeyframeSamples, row.Index)
			}
		}
		if res.Recovered.Samples == 0 {
			res.Recovered.StartMS = row.DTSMS
			res.Recovered.FirstSample = row.Index
		}
		res.Recovered.Samples++
		res.Recovered.Bytes += int64(row.Size)
		res.Recovered.LastSample = row.Index
		if row.EndDTSMS > res.Recovered.EndMS {
			res.Recovered.EndMS = row.EndDTSMS
		}
	}
	if dam != nil {
		// Damage ran to the end of the declared timeline: the tail is
		// unrecoverable up to the track's declared duration.
		trackEnd := clock.AddMS(tr.DurationMS, tr.StartMS)
		if trackEnd < dam.StartMS {
			trackEnd = dam.StartMS
		}
		resumeSpan(trackEnd)
	}

	res.Recovered.Complete = len(res.Damages) == 0
	if res.Recovered.Samples == 0 {
		res.Status = StatusUnrecoverable
		res.addIssue(fault.ForTrack(fault.CodeUnrecoverable, fault.SeverityFatal, "repair", tr.ID,
			res.FirstUnreadable, "no sample could be recovered"), cfg.MaxIssues)
		return res
	}
	if len(res.Damages) > 0 {
		res.Status = StatusRecovered
	} else {
		res.Status = StatusOK
	}
	return res
}

// maxKeyframeTimes bounds retained keyframe boundaries per track.
const maxKeyframeTimes = 1 << 16

// exclude accounts for one sample kept out of the manifest.
func exclude(res *TrackResult, row probe.Row) {
	res.ExcludedSamples++
	if row.Size > 0 {
		res.DiscardedBytes += int64(row.Size)
	}
	if res.FirstUnreadable < 0 {
		res.FirstUnreadable = row.Offset
	}
}

// appendGap records consecutive keyframe spacing within a fixed cap.
func appendGap(gaps []int64, prevMS, curMS int64) []int64 {
	if prevMS < 0 {
		return gaps
	}
	if g := curMS - prevMS; g >= 0 && len(gaps) < 1<<16 {
		return append(gaps, g)
	}
	return gaps
}

// validateRow checks the sample payload. It returns a reason string and the
// classification code, or "" when the row is acceptable.
func validateRow(r *reader, row probe.Row, tr *probe.Track, cfg Config) (string, fault.Code) {
	if !row.InBounds {
		return "sample range escapes the file", fault.CodeTruncated
	}
	if !row.InMdat {
		if row.Offset < 0 || row.Size == 0 {
			return "sample range is outside any mdat payload", fault.CodeCorrupt
		}
		return "sample range is outside any mdat payload", fault.CodeCorrupt
	}
	if row.Size == 0 {
		return "zero-length sample", fault.CodeCorrupt
	}
	if !cfg.Recover || !cfg.DetectUniformFill || int(row.Size) > int(cfg.CacheBytes) {
		return "", ""
	}
	buf, err := r.read(row.Offset, int(row.Size))
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return "sample payload is cut off", fault.CodeTruncated
		}
		if errors.Is(err, errRange) {
			return "sample range rejected before read", fault.CodeTruncated
		}
		return fmt.Sprintf("read failed: %v", err), fault.CodeIO
	}
	if isUniformFill(buf, cfg.FillBytes) {
		return "sample payload is uniform fill", fault.CodeCorrupt
	}
	return "", ""
}

// isUniformFill reports whether every byte equals one of the fill values,
// which is how interrupted writes and padding appear.
func isUniformFill(buf []byte, fill []byte) bool {
	if len(buf) == 0 {
		return false
	}
	var seen byte
	have := false
	for _, b := range buf {
		if !have {
			if !contains(fill, b) {
				return false
			}
			seen = b
			have = true
			continue
		}
		if b != seen {
			return false
		}
	}
	return have
}

func contains(set []byte, b byte) bool {
	for _, v := range set {
		if v == b {
			return true
		}
	}
	return false
}

// reader performs bounded reads with a reusable buffer.
type reader struct {
	file *os.File
	size int64
	buf  []byte
	max  int
}

var errRange = errors.New("range outside file")

func newReader(f *os.File, size int64, max int) *reader {
	if max <= 0 {
		max = 1 << 20
	}
	return &reader{file: f, size: size, max: max}
}

func (r *reader) read(off int64, n int) ([]byte, error) {
	if off < 0 || n < 0 || off > r.size || int64(n) > r.size-off {
		return nil, errRange
	}
	if n > r.max {
		return nil, errRange
	}
	if cap(r.buf) < n {
		r.buf = make([]byte, n)
	}
	buf := r.buf[:n]
	if _, err := r.file.ReadAt(buf, off); err != nil {
		return nil, err
	}
	return buf, nil
}
