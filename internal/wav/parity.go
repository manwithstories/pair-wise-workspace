package wav

import (
	"bytes"
	"hash/crc32"
)

func crc32Of(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

// GenerateParity renders a carrier whose last block of every group is an
// XOR parity block: for each group of dataPerGroup PCM blocks, one extra
// block is appended whose bytes are the XOR of its members. The returned
// total block count includes the parity blocks. Damage lists reference
// indices in this extended block numbering.
func GenerateParity(cfg GenerateConfig, dataPerGroup int) ([]byte, int, error) {
	if err := validateGen(cfg); err != nil {
		return nil, 0, err
	}
	synth := cfg.Synth
	if synth == nil {
		synth = SynthTone
	}
	align := cfg.Channels * int(cfg.BitDepth) / 8
	pcm := make([]byte, cfg.Frames*align)
	for frame := 0; frame < cfg.Frames; frame++ {
		for ch := 0; ch < cfg.Channels; ch++ {
			off := (frame*cfg.Channels + ch) * (int(cfg.BitDepth) / 8)
			EncodeSample(pcm[off:off+int(cfg.BitDepth)/8],
				synth(frame, ch, cfg.Seed), cfg.BitDepth)
		}
	}

	dataBlocks := (cfg.Frames + cfg.BlockFrames - 1) / cfg.BlockFrames
	blockBytes := cfg.BlockFrames * align
	groups := BuildGroupLayout(dataBlocks, dataPerGroup)
	total := dataBlocks + len(groups)
	crcs := make([]uint32, total)

	// PCM data block CRCs at their original indices.
	for i := 0; i < dataBlocks; i++ {
		start := i * blockBytes
		end := min((i+1)*blockBytes, len(pcm))
		crcs[i] = crc32Of(pcm[start:end])
	}
	for _, idx := range cfg.CorruptBlocks {
		if idx >= 0 && idx < dataBlocks {
			start := idx * blockBytes
			if start < len(pcm) {
				pcm[start] ^= 0x5a
			}
		}
	}

	drop := indexSet(cfg.DropBlocks)
	var data bytes.Buffer
	for i := 0; i < dataBlocks; i++ {
		if drop[i] {
			continue
		}
		start := i * blockBytes
		end := min((i+1)*blockBytes, len(pcm))
		data.Write(pcm[start:end])
	}
	for _, g := range groups {
		parity := make([]byte, blockBytes)
		for _, mi := range g.Members {
			start := mi * blockBytes
			end := min((mi+1)*blockBytes, len(pcm))
			for j := start; j < end; j++ {
				parity[j-start] ^= pcm[j]
			}
		}
		pIdx := g.Parity
		crcs[pIdx] = crc32Of(parity)
		if !drop[pIdx] {
			data.Write(parity)
		}
	}

	extra := make([]uint32, 0, len(groups)+1)
	extra = append(extra, uint32(dataBlocks))
	for _, g := range groups {
		extra = append(extra, uint32(g.Parity))
	}
	rsid := encodeManifest(cfg, crcs, extra...)
	if len(rsid)%2 == 1 {
		rsid = append(rsid, 0)
	}
	var out bytes.Buffer
	writeHeader(&out, cfg, data.Len(), rsid)
	writeSideChunk(&out, ManifestID, rsid)
	writeDataChunk(&out, data.Bytes())
	raw := out.Bytes()
	if cfg.TruncateBytes > 0 && cfg.TruncateBytes < len(raw) {
		raw = raw[:len(raw)-cfg.TruncateBytes]
	}
	return raw, total, nil
}

// GroupLayout describes one XOR protection domain in extended numbering.
type GroupLayout struct {
	ID      int
	Members []int // PCM block indices
	Parity  int   // parity block index (after all PCM blocks)
}

// BuildGroupLayout assigns consecutive PCM blocks to groups of at most
// dataPerGroup members; parity blocks are numbered after the PCM range.
func BuildGroupLayout(dataBlocks, dataPerGroup int) []GroupLayout {
	if dataPerGroup <= 0 {
		return nil
	}
	var groups []GroupLayout
	for start, g := 0, 0; start < dataBlocks; start, g = start+dataPerGroup, g+1 {
		end := min(start+dataPerGroup, dataBlocks)
		members := make([]int, 0, end-start)
		for i := start; i < end; i++ {
			members = append(members, i)
		}
		groups = append(groups, GroupLayout{
			ID: g, Members: members, Parity: dataBlocks + g,
		})
	}
	return groups
}
