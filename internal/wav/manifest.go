package wav

import (
	"encoding/binary"
)

// encodeManifest serializes the restore manifest. Layout must mirror
// parseManifest in read.go: magic, version, block height, two length-
// prefixed tags, the missing-index list, then the per-block CRC table.
func encodeManifest(cfg GenerateConfig, crcs []uint32) []byte {
	drop := cfg.DropBlocks
	buf := make([]byte, 0, 12+len(drop)*4+len(crcs)*4)
	buf = append(buf, manifestMagic...)
	buf = binary.LittleEndian.AppendUint16(buf, manifestVersion)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(cfg.BlockFrames))
	buf = append(buf, byte(len(cfg.Layout)))
	buf = append(buf, cfg.Layout...)
	buf = append(buf, byte(len(cfg.Color)))
	buf = append(buf, cfg.Color...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(drop)))
	for _, idx := range drop {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(idx))
	}
	for _, c := range crcs {
		buf = binary.LittleEndian.AppendUint32(buf, c)
	}
	return buf
}
