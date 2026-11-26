package wav

import (
	"encoding/binary"
)

// encodeManifest serializes the restore manifest. Layout must mirror
// parseManifest in read.go: magic, version, block height, two length-
// prefixed tags, the missing-index list, the per-block CRC table, then
// optional parity metadata (data-block count, parity indices).
func encodeManifest(cfg GenerateConfig, crcs []uint32, extra ...uint32) []byte {
	drop := cfg.DropBlocks
	buf := make([]byte, 0, 12+len(drop)*4+len(crcs)*4+len(extra)*4)
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
	for _, v := range extra {
		buf = binary.LittleEndian.AppendUint32(buf, v)
	}
	return buf
}
