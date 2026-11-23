package checkpoint

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
)

// Load verifies the index against the block files. Every block is
// self-describing, so when the index is damaged or disagrees with the blocks it
// is rebuilt from the intact prefix and the store resumes from there.
func (s *Store) Load() (valid int, recovered bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.BlockSize <= 0 || s.BlockSize > maxBlockBytes {
		s.BlockSize = maxBlockBytes
	}
	raw, readErr := os.ReadFile(s.indexPath())
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return 0, false, readErr
	}

	var declared []Entry
	var declaredBlockSize uint32
	if readErr == nil {
		hdr, entries, parseErr := parseIndex(raw)
		if parseErr != nil {
			// Rebuild from block headers rather than failing the run.
			return s.rebuildFromBlocks(0)
		}
		declared, declaredBlockSize = entries, hdr.BlockSize
	}

	// Walk blocks in order; stop at the first missing or damaged one. When the
	// index survived, it must agree with each block header.
	var chain [32]byte
	i := uint32(0)
	for {
		blockRaw, err := os.ReadFile(s.blockPath(i))
		if err != nil {
			break
		}
		if len(blockRaw) > maxBlockBytes+blockHeaderSize {
			break
		}
		index, samples, bytes, crc, payload, ok := decodeBlock(blockRaw)
		if !ok || index != i {
			break
		}
		if crc32.ChecksumIEEE(payload) != crc {
			break
		}
		h := sha256.New()
		h.Write(chain[:])
		h.Write(payload)
		var want [32]byte
		copy(want[:], h.Sum(nil))
		e := Entry{Index: i, Samples: samples, Bytes: bytes, CRC32: crc, ChainHash: want}
		if declared != nil && int(i) < len(declared) {
			d := declared[i]
			if d.CRC32 != crc || d.Samples != samples || d.Bytes != bytes || d.ChainHash != want {
				// Index disagrees with disk; keep the on-disk truth and cut here.
				break
			}
		}
		chain = want
		s.entries = append(s.entries, e)
		s.blocks[i] = append([]byte(nil), payload...)
		s.samples += uint64(samples)
		s.bytes += bytes
		i++
	}
	s.chain = chain

	if declared != nil && int(i) == len(declared) {
		if declaredBlockSize > 0 && declaredBlockSize <= maxBlockBytes {
			s.BlockSize = int(declaredBlockSize)
		}
		return int(i), false, nil
	}
	// Either the index was damaged, or it claims blocks that are missing or
	// inconsistent. Rebuild it from the verified prefix and drop the tail.
	if len(s.entries) > 0 || readErr == nil {
		s.truncateTo(len(s.entries))
		if err := s.writeIndex(); err != nil {
			return len(s.entries), true, err
		}
	}
	return len(s.entries), true, nil
}

// rebuildFromBlocks reconstructs the index when the index file itself cannot be
// parsed. requestedBlockSize is the caller's preferred cap; the block data is
// otherwise authoritative.
func (s *Store) rebuildFromBlocks(requestedBlockSize int) (int, bool, error) {
	if requestedBlockSize > 0 && requestedBlockSize <= maxBlockBytes {
		s.BlockSize = requestedBlockSize
	}
	var chain [32]byte
	i := uint32(0)
	for {
		blockRaw, err := os.ReadFile(s.blockPath(i))
		if err != nil {
			break
		}
		index, samples, bytes, crc, payload, ok := decodeBlock(blockRaw)
		if !ok || index != i || crc32.ChecksumIEEE(payload) != crc {
			break
		}
		h := sha256.New()
		h.Write(chain[:])
		h.Write(payload)
		var want [32]byte
		copy(want[:], h.Sum(nil))
		e := Entry{Index: i, Samples: samples, Bytes: bytes, CRC32: crc, ChainHash: want}
		chain = want
		s.entries = append(s.entries, e)
		s.blocks[i] = append([]byte(nil), payload...)
		s.samples += uint64(samples)
		s.bytes += bytes
		i++
	}
	s.chain = chain
	if err := s.writeIndex(); err != nil {
		return 0, false, err
	}
	return len(s.entries), true, nil
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
