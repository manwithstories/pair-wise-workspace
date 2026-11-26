package wav

import (
	"hash/crc32"
	"io"

	"audiorestore/internal/model"
)

// ReadBlocks streams the PCM payload as fixed-height FrameBlocks. Missing
// blocks (declared by the manifest) are emitted with nil data; present
// blocks are CRC-checked against the manifest, and a short final read is
// marked Truncated rather than padded with fake samples. The log receives
// one structured entry per defect but processing always runs to the end.
func (r *Reader) ReadBlocks(log *model.ErrorLog) ([]FrameBlock, error) {
	blockFrames := r.BlockFrames()
	var totalBlocks int
	if r.manifest != nil && len(r.manifest.CRC) > 0 {
		// The manifest is authoritative: dropped blocks leave no PCM
		// bytes, so the payload length alone cannot count blocks.
		totalBlocks = len(r.manifest.CRC)
	} else {
		totalBlocks = int((r.TotalFrames() + int64(blockFrames) - 1) / int64(blockFrames))
	}
	missing := r.missingSet()

	blocks := make([]FrameBlock, 0, totalBlocks)
	for idx := 0; idx < totalBlocks; idx++ {
		b := FrameBlock{
			Index:      idx,
			StartFrame: int64(idx * blockFrames),
			Frames:     blockFrames,
		}
		if idx == totalBlocks-1 {
			if rem := int(r.TotalFrames() - b.StartFrame); rem < blockFrames {
				b.Frames = rem
			}
		}
		if r.manifest != nil && idx < len(r.manifest.CRC) {
			b.WantCRC = r.manifest.CRC[idx]
		}
		want := b.Frames * r.format.BlockAlign

		if missing[idx] {
			b.Missing = true
			blocks = append(blocks, b)
			log.Addf(model.ErrBlockMissing, "wav", idx, b.StartFrame,
				"block absent from carrier")
			continue
		}

		buf := make([]byte, want)
		n, err := io.ReadFull(r.data, buf)
		if n < want {
			b.Truncated = true
			b.Frames = n / r.format.BlockAlign
			b.Data = nil
			blocks = append(blocks, b)
			log.Addf(model.ErrTruncated, "wav", idx,
				b.StartFrame+int64(b.Frames), "short PCM read: %d/%d bytes", n, want)
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				return blocks, nil
			}
			return blocks, err
		}
		b.Data = buf
		b.CRC = crc32.ChecksumIEEE(buf)
		if b.WantCRC != 0 && b.CRC != b.WantCRC {
			b.Corrupt = true
			log.Addf(model.ErrBlockCRC, "wav", idx, b.StartFrame,
				"crc mismatch: got %08x want %08x", b.CRC, b.WantCRC)
		}
		blocks = append(blocks, b)
		r.read += int64(n)
		if err == io.EOF {
			break
		}
	}
	return blocks, nil
}

func (r *Reader) missingSet() map[int]bool {
	if r.manifest == nil {
		return nil
	}
	set := make(map[int]bool, len(r.manifest.Missing))
	for _, i := range r.manifest.Missing {
		set[i] = true
	}
	return set
}
