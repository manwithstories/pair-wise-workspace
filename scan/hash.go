package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// ioChunkSize bounds the memory used by every streaming hash/copy operation
// in the pipeline; 64 KiB is far below the 256 MiB process budget.
const ioChunkSize = 64 * 1024

func copyChunk(h hash.Hash, r io.Reader) (int64, error) {
	buf := make([]byte, ioChunkSize)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// HashBytes returns the hex SHA-256 of b.
func HashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
