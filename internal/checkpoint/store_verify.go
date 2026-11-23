package checkpoint

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"

	"mediasync/internal/model"
)

// Load reads an existing index, verifying the chain and every block checksum.
// It returns the number of valid blocks and, on corruption, the last valid
// block index so the caller can resume from there.
func (s *Store) Load() (valid int, recovered bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, readErr := os.ReadFile(s.indexPath())
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, readErr
	}
	hdr, entries, parseErr := parseIndex(raw)
	if parseErr != nil {
		return 0, false, model.WrapError(model.ErrCheckpointCorrupt, 0, 0, parseErr)
	}
	s.BlockSize = int(hdr.BlockSize)
	if s.BlockSize <= 0 || s.BlockSize > maxBlockBytes {
		return 0, false, model.NewError(model.ErrCheckpointCorrupt, 0, 0, "index block size %d out of range", hdr.BlockSize)
	}
	var chain [32]byte
	for i, e := range entries {
		data, err := os.ReadFile(s.blockPath(uint32(i)))
		if err != nil {
			s.truncateTo(i)
			return i, true, nil
		}
		if len(data) > maxBlockBytes {
			s.truncateTo(i)
			return i, true, nil
		}
		if crc32.ChecksumIEEE(data) != e.CRC32 {
			s.truncateTo(i)
			return i, true, nil
		}
		h := sha256.New()
		h.Write(chain[:])
		h.Write(data)
		var want [32]byte
		copy(want[:], h.Sum(nil))
		if want != e.ChainHash {
			s.truncateTo(i)
			return i, true, nil
		}
		chain = want
		s.entries = append(s.entries, e)
		s.blocks[uint32(i)] = data
		s.samples += uint64(e.Samples)
		s.bytes += e.Bytes
	}
	s.chain = chain
	return len(entries), false, nil
}

// truncateTo drops entries from index i onwards so a subsequent Append
// continues from the last intact block.
func (s *Store) truncateTo(i int) {
	s.entries = s.entries[:i]
	s.samples = 0
	s.bytes = 0
	var chain [32]byte
	for _, e := range s.entries {
		s.samples += uint64(e.Samples)
		s.bytes += e.Bytes
		chain = e.ChainHash
	}
	s.chain = chain
	// Remove any orphan block files past the cut.
	matches, _ := filepath.Glob(filepath.Join(s.dir, "block-*.bin"))
	sort.Strings(matches)
	for _, m := range matches {
		base := filepath.Base(m)
		var n uint32
		if _, err := fmt.Sscanf(base, "block-%06d.bin", &n); err != nil {
			continue
		}
		if int(n) >= i {
			_ = os.Remove(m)
		}
	}
}

// Stats returns the committed sample and byte counts.
func (s *Store) Stats() (samples, bytes uint64, blocks int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples, s.bytes, len(s.entries)
}

// Chain returns the cumulative hash of all committed blocks.
func (s *Store) Chain() [32]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chain
}

// Entries returns a copy of the index entries.
func (s *Store) Entries() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Entry(nil), s.entries...)
}

// BlockData returns a committed block's bytes.
func (s *Store) BlockData(i uint32) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blocks[i]
	return b, ok
}

// Reset removes every block and the index.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	matches, _ := filepath.Glob(filepath.Join(s.dir, "block-*.bin"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			return err
		}
	}
	if err := os.Remove(s.indexPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.entries = nil
	s.blocks = map[uint32][]byte{}
	s.chain = [32]byte{}
	s.samples = 0
	s.bytes = 0
	return nil
}
