package framehash

import (
	"crypto/sha256"

	"mp4replay/internal/mp4atom"
)

// FindSourceStart returns the source sample index whose bytes match the first
// sample of the clip track, or -1 if no match exists. Reads are on demand and
// bounded to one sample at a time.
func FindSourceStart(src, clip *mp4atom.Movie,
	srcTr, clipTr *mp4atom.Track) (int, error) {
	if len(clipTr.Samples) == 0 {
		return -1, nil
	}
	first := clipTr.Samples[0]
	want, err := readSample(clip.ReaderAt(), first.Offset, first.Size)
	if err != nil {
		return -1, err
	}
	wantSum := sha256.Sum256(want)
	for i, s := range srcTr.Samples {
		if s.Size != first.Size {
			continue
		}
		got, err := readSample(src.ReaderAt(), s.Offset, s.Size)
		if err != nil {
			return -1, err
		}
		if sha256.Sum256(got) == wantSum {
			return i, nil
		}
	}
	return -1, nil
}

// FindSourceStartBySize aligns the clip using the leading sample-size pattern
// rather than content. This still locates the correct source interval when the
// first frame has been corrupted, so per-frame mismatch accounting works.
// Returns -1 when no size pattern matches.
func FindSourceStartBySize(srcTr, clipTr *mp4atom.Track) int {
	clip := clipTr.Samples
	src := srcTr.Samples
	if len(clip) == 0 || len(src) < len(clip) {
		return -1
	}
	need := len(clip)
	for start := 0; start+need <= len(src); start++ {
		ok := true
		for k := 0; k < need; k++ {
			if src[start+k].Size != clip[k].Size {
				ok = false
				break
			}
		}
		if ok {
			return start
		}
	}
	return -1
}
