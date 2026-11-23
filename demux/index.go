package demux

import (
	"fmt"
	"io"

	"bcast/media"
)

// BuildKeyIndex ensures every video sample carries a keyframe flag. When
// the container declared an stss box the flags are already set; otherwise
// the index is rebuilt by scanning sample payloads for IDR access units
// and the samples are marked with KeySrcRebuilt. When payloads cannot be
// read the downstream is blocked on the sample data dependency.
func BuildKeyIndex(f *File, r io.ReaderAt) error {
	vt, ok := f.TrackByKind(media.TrackVideo)
	if !ok {
		return nil // no video track: nothing to index
	}
	samples := f.SamplesOf(vt.ID)
	if len(samples) == 0 {
		return media.Blocked("index.build", "demux.samples",
			fmt.Errorf("%w: video track has no samples", media.ErrIncompleteIndex))
	}
	if samples[0].KeySource == media.KeySrcSTSS {
		return nil // container-declared index is authoritative
	}
	rebuilt := 0
	for i := range f.Samples {
		s := &f.Samples[i]
		if s.TrackID != vt.ID {
			continue
		}
		idr, err := isIDR(r, s.Offset, int64(s.Size))
		if err != nil {
			return media.Blocked("index.build", "demux.samples",
				fmt.Errorf("cannot rebuild keyframe index: %v", err))
		}
		s.Keyframe = idr
		s.KeySource = media.KeySrcRebuilt
		if idr {
			rebuilt++
		}
	}
	if rebuilt == 0 {
		return media.Blocked("index.build", "demux.samples",
			fmt.Errorf("%w: no keyframes recoverable", media.ErrIncompleteIndex))
	}
	return nil
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

// KeyIndexStats summarizes the keyframe index of the video track.
func KeyIndexStats(f *File) (total, keys int, source media.KeyframeSource) {
	vt, ok := f.TrackByKind(media.TrackVideo)
	if !ok {
		return 0, 0, media.KeySrcNone
	}
	for _, s := range f.SamplesOf(vt.ID) {
		total++
		if s.Keyframe {
			keys++
		}
		source = s.KeySource
	}
	return total, keys, source
}
