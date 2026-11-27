package checkpoint

import (
	"encoding/hex"

	"relay/internal/media"
)

// EncodeDedup 将内存账本（二进制哈希）转为检查点的十六进制形式。
func EncodeDedup(in map[uint32]map[uint64][32]byte) map[uint32]map[uint64]string {
	out := make(map[uint32]map[uint64]string, len(in))
	for track, ledger := range in {
		cp := make(map[uint64]string, len(ledger))
		for seq, h := range ledger {
			cp[seq] = hex.EncodeToString(h[:])
		}
		out[track] = cp
	}
	return out
}

// DecodeDedup 将检查点账本还原为二进制形式。
func DecodeDedup(in map[uint32]map[uint64]string) (map[uint32]map[uint64][32]byte, error) {
	out := make(map[uint32]map[uint64][32]byte, len(in))
	for track, ledger := range in {
		cp := make(map[uint64][32]byte, len(ledger))
		for seq, hs := range ledger {
			raw, err := hex.DecodeString(hs)
			if err != nil {
				return nil, media.NewFault(media.StageCheckpoint, 0, 0, seq,
					"bad ledger hash: %v", err)
			}
			if len(raw) != 32 {
				return nil, media.NewFault(media.StageCheckpoint, 0, 0, seq,
					"ledger hash length %d", len(raw))
			}
			var h [32]byte
			copy(h[:], raw)
			cp[seq] = h
		}
		out[track] = cp
	}
	return out, nil
}
