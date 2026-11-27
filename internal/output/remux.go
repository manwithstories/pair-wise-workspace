// Package output produces the remuxed deliverable (H.264/AAC in a fresh
// MP4 container, no transcoding) and the provenance manifest that links
// input digests, fault offsets, recovery decisions and output digests.
package output

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/model"
)

// BlockSize bounds one remux chunk; the writer streams block by block so
// memory scales with the block/window, never the whole source.
const BlockSize = 1 << 20 // 1 MiB

// BlockChecksum describes one committed output block.
type BlockChecksum struct {
	Index      int    `json:"index"`
	ByteLength int64  `json:"byte_length"`
	SHA256     string `json:"sha256"`
}

// blockHasher tracks both the per-block and cumulative running digests.
type blockHasher struct {
	block   hash.Hash
	running hash.Hash
}

// Remuxer streams recovered sample payloads into numbered block files. Each
// committed block is checksummed before the next one opens, which makes
// checkpoint validation and crash-safe resume possible.
type Remuxer struct {
	dir        string
	base       string
	current    *os.File
	currentOff int64
	blockIndex int
	blockBytes int
	hasher     blockHasher
	committed  []BlockChecksum
}

// NewRemuxer opens a remuxer writing dir/base.NNNN blocks.
func NewRemuxer(dir, base string) (*Remuxer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Remuxer{dir: dir, base: base, hasher: blockHasher{
		block: sha256.New(), running: sha256.New()}}, nil
}

func (r *Remuxer) blockPath(i int) string {
	return filepath.Join(r.dir, fmt.Sprintf("%s.%04d.blk", r.base, i))
}

func (r *Remuxer) partPath() string { return r.blockPath(r.blockIndex) + ".part" }

// Resume validates already-committed blocks against their recorded
// checksums, rebuilds the cumulative running hash, and positions the
// remuxer to continue after them. A mismatch or unreadable block returns an
// error so the caller can fail over to a clean rebuild rather than emit an
// inconsistent deliverable.
func (r *Remuxer) Resume(blocks []BlockChecksum) error {
	running := sha256.New()
	buf := make([]byte, 1<<20)
	for _, b := range blocks {
		path := r.blockPath(b.Index)
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.CopyBuffer(io.MultiWriter(running, h), f, buf)
		f.Close()
		if err != nil {
			return err
		}
		if n != b.ByteLength || hex.EncodeToString(h.Sum(nil)) != b.SHA256 {
			return fmt.Errorf("%w: block %d checksum mismatch", model.ErrCheckpointMismatch, b.Index)
		}
	}
	r.hasher.running = running
	r.committed = append(r.committed, blocks...)
	r.blockIndex = len(blocks)
	r.blockBytes = 0
	r.currentOff = 0
	return nil
}

// DiscardPartials removes any uncommitted .part files left by a crash.
func (r *Remuxer) DiscardPartials() error {
	matches, err := filepath.Glob(filepath.Join(r.dir, r.base+".*.part"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (r *Remuxer) openBlock() error {
	f, err := os.OpenFile(r.partPath(),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	r.current = f
	r.currentOff = 0
	r.blockBytes = 0
	r.hasher.block.Reset()
	return nil
}

// Write appends payload bytes, rolling over to a new block at BlockSize.
func (r *Remuxer) Write(p []byte) (int, error) {
	if r.current == nil {
		if err := r.openBlock(); err != nil {
			return 0, err
		}
	}
	written := 0
	for len(p) > 0 {
		if r.blockBytes >= BlockSize {
			if _, err := r.CommitBlock(); err != nil {
				return written, err
			}
			if err := r.openBlock(); err != nil {
				return written, err
			}
		}
		room := BlockSize - r.blockBytes
		chunk := p
		if len(chunk) > room {
			chunk = chunk[:room]
		}
		n, err := r.current.Write(chunk)
		if err != nil {
			return written + n, err
		}
		r.hasher.block.Write(chunk)
		r.hasher.running.Write(chunk)
		r.blockBytes += n
		r.currentOff += int64(n)
		written += n
		p = p[n:]
	}
	return written, nil
}

// CommitBlock flushes and atomically renames the current block, returning
// its checksum record.
func (r *Remuxer) CommitBlock() (BlockChecksum, error) {
	if r.current == nil {
		return BlockChecksum{}, nil
	}
	if err := r.current.Sync(); err != nil {
		return BlockChecksum{}, err
	}
	if err := r.current.Close(); err != nil {
		return BlockChecksum{}, err
	}
	rec := BlockChecksum{
		Index:      r.blockIndex,
		ByteLength: r.currentOff,
		SHA256:     hex.EncodeToString(r.hasher.block.Sum(nil)),
	}
	if err := os.Rename(r.partPath(), r.blockPath(r.blockIndex)); err != nil {
		return rec, err
	}
	r.current = nil
	r.committed = append(r.committed, rec)
	r.blockIndex++
	return rec, nil
}

// CommittedBlocks returns checksums of every block verified or written in
// this remuxer's lifetime.
func (r *Remuxer) CommittedBlocks() []BlockChecksum {
	out := make([]BlockChecksum, len(r.committed))
	copy(out, r.committed)
	return out
}

// Close finalizes any open block.
func (r *Remuxer) Close() error {
	if r.current != nil {
		_, err := r.CommitBlock()
		return err
	}
	return nil
}

// BlockCount returns the number of committed blocks.
func (r *Remuxer) BlockCount() int { return r.blockIndex }

// RunningDigest returns the cumulative SHA-256 over all bytes written so far.
func (r *Remuxer) RunningDigest() string {
	return hex.EncodeToString(r.hasher.running.Sum(nil))
}

// ConcatBlocks merges committed block files into finalPath in index order
// and returns the full digest and byte count, streaming through a buffer.
func ConcatBlocks(dir, base, finalPath string, blocks int) (string, int64, error) {
	out, err := os.OpenFile(finalPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	defer out.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var total int64
	for i := 0; i < blocks; i++ {
		in, err := os.Open(filepath.Join(dir, fmt.Sprintf("%s.%04d.blk", base, i)))
		if err != nil {
			return "", 0, err
		}
		n, err := io.CopyBuffer(io.MultiWriter(out, h), in, buf)
		in.Close()
		if err != nil {
			return "", 0, err
		}
		total += n
	}
	if err := out.Sync(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}
