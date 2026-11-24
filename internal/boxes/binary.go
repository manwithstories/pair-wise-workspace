package boxes

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

func beUint16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func beUint24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}
func beUint32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func beUint64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

func boxType(b [4]byte) string {
	return string(b[:])
}

// fragmentNoFromName extracts the trailing number from seg-00007.m4s or
// video-00003.m4s.  It falls back to the sorted-file ordinal.
func fragmentNoFromName(path string, ordinal int) int {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	idx := strings.LastIndexAny(base, "-_")
	if idx >= 0 && idx+1 < len(base) {
		if n, err := strconv.Atoi(base[idx+1:]); err == nil && n > 0 {
			return n
		}
	}
	var digits strings.Builder
	for _, r := range base {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		} else if digits.Len() > 0 {
			digits.Reset()
		}
	}
	if digits.Len() > 0 {
		if n, err := strconv.Atoi(digits.String()); err == nil && n > 0 {
			return n
		}
	}
	return ordinal
}

func safeEnd(start, size int64) (int64, error) {
	if size < 0 || start < 0 {
		return 0, fmt.Errorf("negative box geometry")
	}
	if start > (1<<62)-size {
		return 0, fmt.Errorf("box end overflow at %d+%d", start, size)
	}
	return start + size, nil
}
