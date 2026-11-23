package demux

import (
	"fmt"
	"io"

	"bcast/media"
)

// BuildKeyIndex ensures every video sample carries a keyframe flag and
// records on the track how the index was established. When the container
// declared an stss box the flags are already set (KeySrcSTSS); otherwise the
// index is rebuilt by scanning sample payloads for IDR access units
// (KeySrcRebuilt). If nothing can be recovered the downstream is blocked and
// the blocking dependency is named in the returned error.
func BuildKeyIndex(f *File, r io.ReaderAt) error {
	vt, ok := f.TrackByKind(media.TrackVideo)
	if !ok {
		return nil // no video track: nothing to index
	}
	samples := f.SamplesOf(vt.ID)
	if len(samples) == 0 {
		err := media.Blocked("index.build", "demux.samples",
			fmt.Errorf("%w: video track has no samples", media.ErrIncompleteIndex))
		f.AddIssue(media.Issue{Kind: media.IssueIncompleteIndex, Offset: -1, TrackID: vt.ID, Detail: err.Error()})
		return err
	}
	if samples[0].KeySource == media.KeySrcSTSS {
		f.setIndexSource(vt.ID, media.KeySrcSTSS)
		return nil // container-declared index is authoritative
	}
	rebuilt, firstBad := 0, int64(-1)
	for i := range f.Samples {
		s := &f.Samples[i]
		if s.TrackID != vt.ID {
			continue
		}
		idr, err := isIDR(r, s.Offset, int64(s.Size))
		if err != nil {
			// A payload that cannot be read is not silently treated as a
			// non-keyframe: the index stays unknown for this sample.
			s.Keyframe, s.KeySource = false, media.KeySrcNone
			if firstBad < 0 {
				firstBad = s.Offset
			}
			continue
		}
		s.Keyframe = idr
		s.KeySource = media.KeySrcRebuilt
		if idr {
			rebuilt++
		}
	}
	if rebuilt == 0 {
		err := media.Blocked("index.build", "demux.samples",
			fmt.Errorf("%w: no keyframes recoverable", media.ErrIncompleteIndex))
		f.AddIssue(media.Issue{Kind: media.IssueIncompleteIndex, Offset: firstBad, TrackID: vt.ID, Detail: err.Error()})
		return err
	}
	if firstBad >= 0 {
		// The index is usable for the salvaged prefix but incomplete; record
		// the damage so the report cannot present it as a clean rebuild.
		f.AddIssue(media.Issue{
			Kind: media.IssueTruncated, Offset: firstBad, TrackID: vt.ID,
			Detail: "keyframe index rebuilt only for readable samples",
		})
	}
	f.setIndexSource(vt.ID, media.KeySrcRebuilt)
	return nil
}

// setIndexSource stamps the index provenance on the track entry.
func (f *File) setIndexSource(trackID int, src media.KeyframeSource) {
	for i := range f.Tracks {
		if f.Tracks[i].ID == trackID {
			f.Tracks[i].IndexFrom = src
		}
	}
}

// isIDR reports whether the sample at [off, off+size) starts with an IDR
// NAL unit (AVC length-prefixed framing, NAL type 5).
func isIDR(r io.ReaderAt, off, size int64) (bool, error) {
	if size < 5 {
		return false, nil
	}
	var hdr [5]byte
	if _, err := r.ReadAt(hdr[:], off); err != nil {
		return false, err
	}
	nalLen := int64(hdr[0])<<24 | int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
	if nalLen < 1 || nalLen > size-4 {
		return false, nil
	}
	return hdr[4]&0x1f == 5, nil
}

// KeyIndexStats summarizes the keyframe index of the video track. Source is
// the track's recorded provenance (media.KeySrcNone when never built).
func KeyIndexStats(f *File) (total, keys int, source media.KeyframeSource) {
	vt, ok := f.TrackByKind(media.TrackVideo)
	if !ok {
		return 0, 0, media.KeySrcNone
	}
	source = vt.IndexFrom
	for _, s := range f.SamplesOf(vt.ID) {
		total++
		if s.Keyframe {
			keys++
		}
	}
	return total, keys, source
}
