package segment

import (
	"encoding/binary"
	"fmt"
)

// Codec identifies the bitstream flavor carried in the samples.
type Codec string

const (
	CodecAVC  Codec = "avc1"
	CodecHEVC Codec = "hvc1"
)

// SplitAVCC splits one sample into its length-prefixed NAL units
// (4-byte big-endian lengths, AVCC format).
func SplitAVCC(sample []byte) ([][]byte, error) {
	var nals [][]byte
	for len(sample) > 0 {
		if len(sample) < 4 {
			return nil, fmt.Errorf("avcc: dangling %d bytes after last NAL", len(sample))
		}
		n := binary.BigEndian.Uint32(sample)
		sample = sample[4:]
		if uint64(n) > uint64(len(sample)) {
			return nil, fmt.Errorf("avcc: NAL declares %d bytes, %d remain", n, len(sample))
		}
		nals = append(nals, sample[:n])
		sample = sample[n:]
	}
	return nals, nil
}

// IsIDR reports whether the NAL unit is an instantaneous decoder refresh
// for the given codec (AVC type 5; HEVC types 19 and 20).
func IsIDR(codec Codec, nal []byte) bool {
	if len(nal) == 0 {
		return false
	}
	switch codec {
	case CodecAVC:
		return nal[0]&0x1f == 5
	case CodecHEVC:
		t := (nal[0] >> 1) & 0x3f
		return t == 19 || t == 20
	}
	return false
}

// FindKeyframes returns the indices of samples that contain at least one
// IDR NAL unit. Sample data is read from mdat (the mdat payload only).
// A sample whose declared size overruns mdat is reported as truncation.
func FindKeyframes(mdat []byte, samples []Sample, codec Codec) ([]int, error) {
	var hits []int
	off := int64(0)
	for i, s := range samples {
		end := off + int64(s.Size)
		if end > int64(len(mdat)) {
			return hits, &TruncatedError{
				Offset:   off,
				BoxType:  "mdat/sample",
				Declared: end,
				Remain:   int64(len(mdat)),
			}
		}
		nals, err := SplitAVCC(mdat[off:end])
		if err != nil {
			return hits, fmt.Errorf("sample %d: %w", i, err)
		}
		for _, n := range nals {
			if IsIDR(codec, n) {
				hits = append(hits, i)
				break
			}
		}
		off = end
	}
	return hits, nil
}

// KeyframeGaps returns the distances (in samples) between consecutive
// keyframe positions. A single keyframe yields an empty slice.
func KeyframeGaps(keyframes []int) []int {
	var gaps []int
	for i := 1; i < len(keyframes); i++ {
		gaps = append(gaps, keyframes[i]-keyframes[i-1])
	}
	return gaps
}
