package encode

import (
	"encoding/binary"
	"os"

	"mediapipe/internal/media"
)

// PadFileToBudget appends a standards-allowed top-level "free" box so the
// delivered segment size lands on the rate-controller budget. Padding is only
// appended before the atomic rename; it never rewrites media samples.
func PadFileToBudget(path string, targetBytes int64) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, media.WrapError(media.CodeIO, "stat before padding", err)
	}
	current := st.Size()
	boxSize := targetBytes - current
	if boxSize <= 8 {
		return 0, nil
	}
	if boxSize > int64(^uint32(0)) {
		return 0, media.NewError(media.CodeEncode, "padding box too large", nil)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, media.WrapError(media.CodeIO, "open for padding", err)
	}
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[0:4], uint32(boxSize))
	copy(header[4:8], "free")
	if _, err := f.Write(header); err != nil {
		_ = f.Close()
		return 0, media.WrapError(media.CodeIO, "write free header", err)
	}
	zero := make([]byte, 64*1024)
	remaining := boxSize - 8
	for remaining > 0 {
		n := int64(len(zero))
		if n > remaining {
			n = remaining
		}
		if _, err := f.Write(zero[:n]); err != nil {
			_ = f.Close()
			return 0, media.WrapError(media.CodeIO, "write free payload", err)
		}
		remaining -= n
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, media.WrapError(media.CodeIO, "sync padding", err)
	}
	if err := f.Close(); err != nil {
		return 0, media.WrapError(media.CodeIO, "close padded file", err)
	}
	return boxSize, nil
}
