package fingerprint

import (
	"fmt"
	"strings"

	"mediasync/internal/media"
	"mediasync/internal/mp4"
	"mediasync/internal/segment"
	"mediasync/internal/timeline"
)

// Clip is one hashed segment together with the data it was derived from.
type Clip struct {
	Camera  string          `json:"camera"`
	Index   int             `json:"index"`
	StartNS int64           `json:"start_ns"`
	EndNS   int64           `json:"end_ns"`
	Hash    string          `json:"sha256"`
	Bytes   int             `json:"bytes_hashed"`
	Flags   Flags           `json:"flags"`
	Segment segment.Segment `json:"-"`
}

// IncompleteReason renders Flags.Incomplete as a human-readable cause.
func (f Flags) IncompleteReason() string {
	if len(f.MissingRanges) > 0 {
		return "missing " + strings.Join(f.MissingRanges, ",")
	}
	if f.SampleCount < f.ExpectedCount {
		return fmt.Sprintf("samples %d/%d", f.SampleCount, f.ExpectedCount)
	}
	return "flagged incomplete"
}

// Build hashes a segment from a track. The hash covers sample digests (the
// first bytes of each sample, truncated to a stable digest), the transcript
// text and the silence mask, so the same content from any camera matches.
// Completeness is judged from the MP4 index only: packet-stream gaps are
// reported by the segment stage and do not change the content of a sample. A
// clip whose samples fall outside the file can never be hashed, which is what
// keeps dedupe from silently matching on missing data.
func Build(camera string, seg segment.Segment, tr *mp4.Track, fileBytes []byte,
	tb timeline.Timebase, transcript string, silenceNS []int64) Clip {
	clip := Clip{Camera: camera, Index: seg.Index, StartNS: seg.StartNS, EndNS: seg.EndNS, Segment: seg}
	clip.Flags = Flags{Timebase: &tb, SampleCount: 0, ExpectedCount: seg.EndSample - seg.StartSample + 1}
	desc := Description{StartNS: seg.StartNS, EndNS: seg.EndNS, Transcript: transcript, SilenceMask: silenceNS}
	for i := seg.StartSample; i <= seg.EndSample && i < len(tr.Samples); i++ {
		s := tr.Samples[i]
		clip.Flags.SampleCount++
		body := sampleBody(fileBytes, s)
		if body == nil {
			clip.Flags.Incomplete = true
			clip.Flags.MissingRanges = append(clip.Flags.MissingRanges,
				fmt.Sprintf("sample %d at %d", s.Number, s.Offset))
			continue
		}
		desc.Samples = append(desc.Samples, SampleDigest{
			DTSNS:      s.DTSNS,
			DurationNS: s.DurationNS,
			Size:       s.Size,
			Digest:     DigestOfBytes(body),
		})
		clip.Bytes += len(body)
	}
	if clip.Flags.SampleCount < clip.Flags.ExpectedCount {
		clip.Flags.Incomplete = true
	}
	if clip.Flags.Incomplete {
		return clip
	}
	hash, err := desc.Hash()
	if err != nil {
		clip.Flags.Incomplete = true
		clip.Flags.MissingRanges = append(clip.Flags.MissingRanges, err.Error())
		return clip
	}
	clip.Hash = hash
	return clip
}

// sampleBody returns the bytes of one sample, or nil when it lies outside the
// file (an out-of-range index must never be hashed silently).
func sampleBody(file []byte, s mp4.Sample) []byte {
	if s.Offset < 0 || s.Size == 0 {
		return nil
	}
	end := s.Offset + int64(s.Size)
	if end < s.Offset || end > int64(len(file)) {
		return nil
	}
	return file[s.Offset:end]
}

// IssueFor builds a fingerprint issue for an excluded clip.
func IssueFor(clip Clip, format string, args ...any) *media.Issue {
	return media.New(media.StageFingerprint, media.InputMP4, media.CodeIncomplete, format, args...).
		WithRecord(clip.Index + 1)
}
