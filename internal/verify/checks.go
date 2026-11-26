package verify

import (
	"hash/crc32"
	"io"
	"os"

	"archivectl/internal/core"
)

// BlockCheck is a generator-produced region check record.
type BlockCheck struct {
	Index      int    `json:"index"`
	StartFrame int64  `json:"start_frame"`
	FrameCount int    `json:"frame_count"`
	CRC32      uint32 `json:"crc32"`
}

// BlockStatus pairs the expected check with what was found in the WAV.
type BlockStatus struct {
	BlockCheck
	ActualCRC       uint32 `json:"actual_crc32"`
	AvailableFrames int64  `json:"available_frames"`
	OK              bool   `json:"ok"`
	DamageKind      string `json:"damage_kind,omitempty"`
}

// CRC32PCMRegion computes the IEEE CRC of one frame-aligned PCM region.
func CRC32PCMRegion(r io.ReaderAt, dataOffset int64, frameBytes int, startFrame, frameCount int64) (uint32, error) {
	const op = "verify.CRC32PCMRegion"
	nBytes := frameCount * int64(frameBytes)
	if nBytes < 0 {
		return 0, core.Newf(core.KindInvalidArgument, op, "negative byte length")
	}
	if err := core.CheckAllocation(op, int64(64*1024)); err != nil {
		return 0, err
	}
	h := crc32.NewIEEE()
	buf := make([]byte, 64*1024)
	off := dataOffset + startFrame*int64(frameBytes)
	remaining := nBytes
	for remaining > 0 {
		nWant := int64(len(buf))
		if nWant > remaining {
			nWant = remaining
		}
		n, err := r.ReadAt(buf[:nWant], off)
		if n > 0 {
			h.Write(buf[:n])
			off += int64(n)
			remaining -= int64(n)
		}
		if err != nil {
			if err == io.EOF && n == 0 {
				break
			}
			if err != io.EOF {
				return 0, core.Wrap(core.KindIO, op, "", err)
			}
			if n == 0 {
				break
			}
		}
	}
	return h.Sum32(), nil
}

// CheckBlocks compares variable-length block CRCs and converts failures into
// sample-frame damage spans. availableFrames is the readable PCM frame count.
func CheckBlocks(r io.ReaderAt, dataOffset int64, frameBytes int, availableFrames int64, blocks []BlockCheck) ([]BlockStatus, []core.Span, error) {
	const op = "verify.CheckBlocks"
	statuses := make([]BlockStatus, 0, len(blocks))
	var spans []core.Span
	for _, b := range blocks {
		if b.FrameCount < 0 {
			return nil, nil, core.Newf(core.KindInvalidArgument, op, "block %d negative frames", b.Index)
		}
		end := b.StartFrame + int64(b.FrameCount)
		st := BlockStatus{BlockCheck: b}
		switch {
		case b.StartFrame >= availableFrames:
			st.AvailableFrames = 0
			st.DamageKind = "missing_block"
			spans = append(spans, core.Span{
				Kind: "missing_block", StartFrame: b.StartFrame, EndFrame: end,
				Recoverable: int64(b.FrameCount) <= int64(core.MaxCachedFrames),
				Source:      "block-check",
				Detail:      "block absent after truncated/missing data",
			})
		default:
			frames := end - b.StartFrame
			availableInBlock := availableFrames - b.StartFrame
			if availableInBlock < frames {
				frames = availableInBlock
			}
			st.AvailableFrames = frames
			crc, err := CRC32PCMRegion(r, dataOffset, frameBytes, b.StartFrame, frames)
			if err != nil {
				return nil, nil, err
			}
			st.ActualCRC = crc
			if availableInBlock < int64(b.FrameCount) {
				st.DamageKind = "truncated_block"
				spans = append(spans, core.Span{
					Kind: "truncation", StartFrame: b.StartFrame + frames, EndFrame: end,
					Recoverable: int64(b.FrameCount)-frames <= int64(core.MaxCachedFrames),
					Source:      "block-check",
					Detail:      "partial block at EOF",
				})
				// The readable prefix also fails its full-block CRC; mark the
				// whole block for deterministic zero concealment.
				spans = append(spans, core.Span{
					Kind: "crc_mismatch", StartFrame: b.StartFrame, EndFrame: b.StartFrame + frames,
					Recoverable: frames <= int64(core.MaxCachedFrames), Source: "crc32",
					Detail: "partial block CRC cannot match full record",
				})
			} else if crc != b.CRC32 {
				st.DamageKind = "crc_mismatch"
				spans = append(spans, core.Span{
					Kind: "bit_flip_or_crc_mismatch", StartFrame: b.StartFrame, EndFrame: end,
					Recoverable: int64(b.FrameCount) <= int64(core.MaxCachedFrames),
					Source:      "crc32",
					Detail:      "CRC32 mismatch indicates bit corruption",
				})
			} else {
				st.OK = true
			}
		}
		statuses = append(statuses, st)
	}
	return statuses, core.MergeSpans(spans), nil
}

// CheckBlocksPath is the file-backed convenience wrapper.
func CheckBlocksPath(path string, dataOffset int64, frameBytes int, availableFrames int64, blocks []BlockCheck) ([]BlockStatus, []core.Span, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, core.Wrap(core.KindIO, "verify.CheckBlocksPath", path, err)
	}
	defer f.Close()
	return CheckBlocks(f, dataOffset, frameBytes, availableFrames, blocks)
}
