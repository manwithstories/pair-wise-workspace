package chunker

import (
	"bufio"
	"encoding/binary"
	"io"
	"math/bits"
)

const (
	MinSize   = 4 * 1024
	MaxSize   = 128 * 1024
	windowLen = 64
)

// SplitStats describes a completed split.
type SplitStats struct {
	Bytes  uint64
	Chunks int
}

var buzhashTable = makeTable()

func makeTable() [256]uint64 {
	// A fixed SplitMix64 sequence makes chunk boundaries independent of the
	// process, platform, and Go version.
	var table [256]uint64
	x := uint64(0x6a09e667f3bcc909)
	for i := range table {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		table[i] = z ^ (z >> 31)
	}
	return table
}

// Splitter emits content-defined chunks in the inclusive range [4 KiB, 128 KiB].
// It keeps at most one chunk and a small rolling window in memory.
type Splitter struct {
	r     *bufio.Reader
	eof   bool
	stats SplitStats
}

func New(r io.Reader) *Splitter {
	return &Splitter{r: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next chunk. It returns io.EOF after the final chunk.
func (s *Splitter) Next() ([]byte, error) {
	if s.eof {
		return nil, io.EOF
	}
	chunk := make([]byte, 0, MinSize)
	var hash uint64
	var ring [windowLen]byte
	ringPos := 0
	rolled := 0
	// A threshold (rather than a power-of-two mask) targets roughly 32 KiB
	// chunks while keeping the 4-128 KiB hard bounds.
	boundary := ^uint64(0) / 100000

	for {
		b, err := s.r.ReadByte()
		if err != nil {
			if err == io.EOF {
				s.eof = true
				if len(chunk) == 0 {
					return nil, io.EOF
				}
				s.stats.Bytes += uint64(len(chunk))
				s.stats.Chunks++
				return chunk, nil
			}
			return nil, err
		}
		chunk = append(chunk, b)
		hash = bits.RotateLeft64(hash, 1) ^ buzhashTable[b]
		if rolled >= windowLen {
			out := ring[ringPos]
			hash ^= bits.RotateLeft64(buzhashTable[out], windowLen)
		} else {
			rolled++
		}
		ring[ringPos] = b
		ringPos = (ringPos + 1) % windowLen

		if len(chunk) >= MinSize && hash <= boundary {
			s.stats.Bytes += uint64(len(chunk))
			s.stats.Chunks++
			return chunk, nil
		}
		if len(chunk) >= MaxSize {
			s.stats.Bytes += uint64(len(chunk))
			s.stats.Chunks++
			return chunk, nil
		}
	}
}

// Split consumes r and calls fn for each non-empty chunk in order.
func Split(r io.Reader, fn func([]byte) error) (SplitStats, error) {
	s := New(r)
	for {
		chunk, err := s.Next()
		if err == io.EOF {
			return s.stats, nil
		}
		if err != nil {
			return s.stats, err
		}
		if err := fn(chunk); err != nil {
			return s.stats, err
		}
	}
}

// HashWindow is exposed for deterministic boundary tests.
func HashWindow(data []byte) uint64 {
	var hash uint64
	var ring [windowLen]byte
	pos := 0
	rolled := 0
	for _, b := range data {
		hash = bits.RotateLeft64(hash, 1) ^ buzhashTable[b]
		if rolled >= windowLen {
			hash ^= bits.RotateLeft64(buzhashTable[ring[pos]], windowLen)
		} else {
			rolled++
		}
		ring[pos] = b
		pos = (pos + 1) % windowLen
	}
	return hash
}

// EncodeUint64 is a tiny helper used by tests and fixture generators that want
// stable binary input.
func EncodeUint64(v uint64) [8]byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], v)
	return out
}
