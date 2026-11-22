package gen

import (
	"fmt"

	"mediascan/internal/stream"
)

// Corruption kinds selectable from the command line.
const (
	CorruptChecksum    = "checksum"
	CorruptTruncateBox = "truncate-box"
)

// CorruptChecksum flips payload bytes of frame frameIdx in place. The
// recorded manifest hash then no longer matches, exercising the checksum
// path that must hit both quality stats and corruption ranges.
func CorruptChecksumPayload(payload []byte, frameIdx int) error {
	if len(payload) == 0 {
		return fmt.Errorf("gen: cannot corrupt empty payload at frame %d", frameIdx)
	}
	payload[0] ^= 0xFF
	if len(payload) > 1 {
		payload[len(payload)-1] ^= 0x0F
	}
	return nil
}

// TruncatedMP4 builds a minimal valid container and then truncates it in
// the middle of the mdat payload while leaving the declared box header
// intact, so the scanner must locate a truncated-box fault by byte offset.
func TruncatedMP4(fullPayload []byte, dropTail int) ([]byte, error) {
	if dropTail <= 0 || dropTail >= len(fullPayload) {
		return nil, fmt.Errorf("gen: truncation %d must be within payload length %d", dropTail, len(fullPayload))
	}
	container, err := stream.BuildMinimalMP4(fullPayload)
	if err != nil {
		return nil, err
	}
	return container[:len(container)-dropTail], nil
}

// HasCorruption reports whether the given kind list contains a kind.
func HasCorruption(kinds []string, kind string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
