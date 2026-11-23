// Package checkpoint persists resumable progress in fixed-size blocks. Each
// block carries its own checksum and the cumulative hash of all samples written
// so far, so a resumed run can verify it continues from an intact prefix.
package checkpoint

import (
	"crypto/sha256"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"mediasync/internal/model"
)

// maxBlockBytes is the hard per-block budget: 64 MiB.
const maxBlockBytes = 64 << 20

// maxIndexBytes caps the index file so a corrupt length cannot exhaust memory.
const maxIndexBytes = 8 << 20

// Header is the on-disk index prefix.
type Header struct {
	Magic       [8]byte
	Version     uint32
	BlockSize   uint32
	SampleCount uint64
	ByteCount   uint64
	BlockCount  uint32
}

var magic = [8]byte{'M', 'S', 'Y', 'N', 'C', 'C', 'K', 'P'}

// Entry describes one persisted block in the index.
type Entry struct {
	Index   uint32
	Samples uint32
	Bytes   uint64
	CRC32   uint32
	// ChainHash is the cumulative SHA-256 after this block.
	ChainHash [32]byte
}

// Block is one block's payload and metadata.
type Block struct {
	Entry
	TrackID uint32
	Data    []byte
}

// Store is a directory-backed checkpoint set.
type Store struct {
	dir       string
	BlockSize int
	mu        sync.Mutex
	entries   []Entry
	chain     [32]byte
	samples   uint64
	bytes     uint64
	// blocks holds payloads queued in this process, kept for index integrity
	// checks and for tests; it is not the source of truth across restarts.
	blocks map[uint32][]byte
}

// Open creates a store rooted at dir. A missing directory is created.
func Open(dir string) (*Store, error) {
	clean, err := safeDir(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: clean, BlockSize: maxBlockBytes, blocks: map[uint32][]byte{}}, nil
}

// safeDir rejects path escapes and absolute parents that leave the working
// tree when the caller used a relative path with "..".
func safeDir(dir string) (string, error) {
	if dir == "" {
		return "", model.NewError(model.ErrParamInvalid, 0, 0, "empty checkpoint directory")
	}
	clean := filepath.Clean(dir)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", model.NewError(model.ErrPathEscape, 0, 0, "checkpoint directory escapes working tree: %s", dir)
	}
	return clean, nil
}

// Dir returns the resolved directory.
func (s *Store) Dir() string { return s.dir }

// Paths returns the block and index paths for an index.
func (s *Store) blockPath(i uint32) string {
	return filepath.Join(s.dir, fmt.Sprintf("block-%06d.bin", i))
}
func (s *Store) indexPath() string { return filepath.Join(s.dir, "index.bin") }

// Append writes one block atomically and updates the index. data is copied
// into a block file named by sequence; a block larger than BlockSize fails.
func (s *Store) Append(trackID uint32, data []byte) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(data) > s.BlockSize || s.BlockSize > maxBlockBytes {
		return Entry{}, model.NewError(model.ErrSizeLimit, trackID, 0, "block of %d bytes exceeds %d byte limit", len(data), s.BlockSize)
	}
	idx := uint32(len(s.entries))
	sum := crc32.ChecksumIEEE(data)
	chain := s.chain
	h := sha256.New()
	h.Write(chain[:])
	h.Write(data)
	copy(chain[:], h.Sum(nil))
	entry := Entry{Index: idx, Samples: 1, Bytes: uint64(len(data)), CRC32: sum, ChainHash: chain}
	if err := writeAtomic(s.blockPath(idx), data); err != nil {
		return Entry{}, err
	}
	s.entries = append(s.entries, entry)
	s.chain = chain
	s.samples++
	s.bytes += uint64(len(data))
	s.blocks[idx] = append([]byte(nil), data...)
	if err := s.writeIndex(); err != nil {
		// The block is durable but the index is not; report so the caller can
		// decide to retry rather than silently claim progress.
		return Entry{}, err
	}
	return entry, nil
}

// AppendSamples writes one block covering n samples with the given payload.
func (s *Store) AppendSamples(trackID uint32, data []byte, samples int) (Entry, error) {
	entry, err := s.Append(trackID, data)
	if err != nil {
		return Entry{}, err
	}
	if samples > 1 {
		s.mu.Lock()
		s.entries[len(s.entries)-1].Samples = uint32(samples)
		s.samples += uint64(samples - 1)
		s.mu.Unlock()
		if err := s.writeIndex(); err != nil {
			return Entry{}, err
		}
	}
	return entry, nil
}
