package fixture

import (
	"io"

	"mediacore/internal/sample"
)

func writeStts(w io.Writer, gt genTrack) {
	_ = FullBox("stts", 0, 0, func(w io.Writer) error {
		u32(w, 1) // one run covering all samples
		u32(w, uint32(gt.spec.Samples))
		u32(w, gt.spec.Delta)
		return nil
	})(w)
}

func writeStsc(w io.Writer, gt genTrack) {
	_ = FullBox("stsc", 0, 0, func(w io.Writer) error {
		u32(w, 1) // every chunk holds all samples of this track
		u32(w, 1) // first chunk
		u32(w, uint32(gt.spec.Samples))
		u32(w, 1) // sample description index
		return nil
	})(w)
}

func writeStsz(w io.Writer, gt genTrack) {
	_ = FullBox("stsz", 0, 0, func(w io.Writer) error {
		u32(w, 0) // uniform size 0 => table follows
		u32(w, uint32(gt.spec.Samples))
		for _, p := range gt.payload {
			u32(w, uint32(sample.FrameHeaderLen+len(p)))
		}
		return nil
	})(w)
}

func writeStss(w io.Writer, gt genTrack) {
	_ = FullBox("stss", 0, 0, func(w io.Writer) error {
		var keys []uint32
		for k := 1; k <= gt.spec.Samples; k++ {
			if gt.spec.KeyEvery > 0 && k%gt.spec.KeyEvery == 0 {
				keys = append(keys, uint32(k))
			}
		}
		u32(w, uint32(len(keys)))
		for _, k := range keys {
			u32(w, k)
		}
		return nil
	})(w)
}

// chunkStart computes the absolute frame offset of this track's single chunk
// by summing preceding tracks' frame sizes within the mdat payload.
func chunkStart(gts []genTrack, idx int, mdatPayloadOff int64) int64 {
	off := mdatPayloadOff
	for j := 0; j < idx; j++ {
		for _, p := range gts[j].payload {
			off += int64(sample.FrameHeaderLen + len(p))
		}
	}
	return off
}

func writeChunkOffsets(w io.Writer, gt genTrack, s Spec, chunkOff int64) {
	if s.Variant == 1 {
		_ = FullBox("co64", 0, 0, func(w io.Writer) error {
			u32(w, 1)
			if s.Mode == ModeIndexOutOfRange {
				u64(w, 0xFFFFFFFFFFFF)
			} else {
				u64(w, uint64(chunkOff))
			}
			return nil
		})(w)
	} else {
		_ = FullBox("stco", 0, 0, func(w io.Writer) error {
			u32(w, 1)
			if s.Mode == ModeIndexOutOfRange {
				u32(w, 0xFFFFFFF0)
			} else {
				u32(w, uint32(chunkOff))
			}
			return nil
		})(w)
	}
}
