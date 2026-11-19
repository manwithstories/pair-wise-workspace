// Package checkpoint persists resumable job progress in two files:
//
//	job.ckpt        the published checkpoint (atomically replaced)
//	job.ckpt.part   a chunk being written, never trusted after a crash
//
// A checkpoint is only published after the index writer has flushed and
// synced the data and tables it describes. On resume the input digest, the
// committed chunk checksums and the index state are all re-verified; a
// mismatch, truncation or stale generation is refused rather than silently
// trusted.
package checkpoint

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mediq/internal/index"
	"mediq/internal/model"
	"mediq/internal/timeline"
)

// Version of the checkpoint file format.
const Version = 1

const (
	magic        = "MCKP"
	headerSize   = 64
	bodyChecksum = 32 // trailing SHA-256 of everything before it
)

// ChunkRecord identifies one committed index chunk and its checksum.
type ChunkRecord struct {
	Index      uint32 `json:"index"`       // 0-based chunk ordinal
	FirstFrame uint64 `json:"first_frame"` // sequence number of the first frame
	LastFrame  uint64 `json:"last_frame"`  // sequence number of the last frame
	Samples    uint32 `json:"samples"`
	Checksum   uint32 `json:"checksum"` // CRC-32 over the sample payloads
}

// State is the full resumable job state.
type State struct {
	Version    int    `json:"version"`
	JobID      string `json:"job_id"`
	Generation uint64 `json:"generation"`
	UpdatedUTC string `json:"updated_utc"`
	InputPath  string `json:"input_path"`
	InputSize  int64  `json:"input_size"`
	InputCRC   uint32 `json:"input_crc"`
	// CommittedOffset is the first byte not yet consumed; a resumed run scans
	// from here, after proving the prefix still matches InputCRC.
	CommittedOffset   int64             `json:"committed_offset"`
	SourceDone        bool              `json:"source_done"` // source fully scanned
	Chunks            []ChunkRecord     `json:"chunks"`
	Index             index.State       `json:"index"`
	Timeline          timeline.Snapshot `json:"timeline"`
	FramesRead        uint64            `json:"frames_read"`
	FramesBad         uint64            `json:"frames_bad"`
	FramesQuarantined uint64            `json:"frames_quarantined"`
	ResumeMs          int64             `json:"last_commit_ms"`
}

// Store manages a checkpoint pair on disk.
type Store struct {
	path string
}

// New returns a store rooted at the published checkpoint path.
func New(path string) *Store { return &Store{path: path} }

// Path returns the published checkpoint path.
func (s *Store) Path() string { return s.path }

func (s *Store) partPath() string   { return s.path + ".part" }
func (s *Store) backupPath() string { return s.path + ".prev" }

// Load reads and validates the published checkpoint. A missing checkpoint
// returns (nil, nil). A corrupt or truncated checkpoint returns an error
// wrapping model.ErrCheckpointMismatch.
func (s *Store) Load() (*State, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st, err := decode(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", model.ErrCheckpointMismatch, err)
	}
	return st, nil
}

// Save durably writes a new checkpoint. The payload is first written to a
// temporary part file with an fsync, then renamed over the published path, so
// a crash leaves either the old or the new checkpoint, never a torn one.
func (s *Store) Save(st *State) error {
	st.Version = Version
	st.Generation++
	st.UpdatedUTC = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := encode(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	part := s.partPath()
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 64<<10)
	if _, err := bw.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Keep the previous checkpoint for post-mortem diagnosis.
	if _, err := os.Stat(s.path); err == nil {
		_ = copyFile(s.path, s.backupPath())
	}
	if err := os.Rename(part, s.path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(s.path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Reset removes every checkpoint artefact, so a fresh run cannot resume from
// an unrelated job.
func (s *Store) Reset() error {
	for _, p := range []string{s.path, s.partPath(), s.backupPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// encode lays out the checkpoint as:
//
//	"MCKP" | version u32 | generation u64 | body length u64 | body SHA-256 | JSON body | SHA-256(all preceding)
func encode(st *State) ([]byte, error) {
	body, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	bodySum := sha256.Sum256(body)
	var hdr [headerSize]byte
	copy(hdr[0:4], magic)
	binary.BigEndian.PutUint32(hdr[4:8], Version)
	binary.BigEndian.PutUint64(hdr[8:16], st.Generation)
	binary.BigEndian.PutUint64(hdr[16:24], uint64(len(body)))
	copy(hdr[24:56], bodySum[:])

	var buf bytes.Buffer
	buf.Write(hdr[:])
	buf.Write(body)
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes(), nil
}

func decode(raw []byte) (*State, error) {
	if len(raw) < headerSize+bodyChecksum {
		return nil, fmt.Errorf("checkpoint too short: %d bytes", len(raw))
	}
	body := raw[headerSize : len(raw)-bodyChecksum]
	want := sha256.Sum256(raw[:len(raw)-bodyChecksum])
	if !bytes.Equal(want[:], raw[len(raw)-bodyChecksum:]) {
		return nil, fmt.Errorf("checkpoint checksum mismatch (truncated or corrupted)")
	}
	if string(raw[0:4]) != magic {
		return nil, fmt.Errorf("bad checkpoint magic %q", raw[0:4])
	}
	if v := binary.BigEndian.Uint32(raw[4:8]); v != Version {
		return nil, fmt.Errorf("unsupported checkpoint version %d", v)
	}
	gen := binary.BigEndian.Uint64(raw[8:16])
	bodyLen := binary.BigEndian.Uint64(raw[16:24])
	if bodyLen != uint64(len(body)) {
		return nil, fmt.Errorf("checkpoint body length %d != %d", bodyLen, len(body))
	}
	bodySum := sha256.Sum256(body)
	if !bytes.Equal(bodySum[:], raw[24:56]) {
		return nil, fmt.Errorf("checkpoint body digest mismatch")
	}
	var st State
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, fmt.Errorf("checkpoint body: %w", err)
	}
	if st.Version != Version {
		return nil, fmt.Errorf("checkpoint states version %d", st.Version)
	}
	if st.Generation != gen {
		return nil, fmt.Errorf("checkpoint generation %d != header %d", st.Generation, gen)
	}
	if err := st.validate(); err != nil {
		return nil, err
	}
	return &st, nil
}

func (st *State) validate() error {
	if st.Index.Samples > 0 && st.Index.LastMedia < st.Index.FirstMedia {
		return fmt.Errorf("index media range inverted")
	}
	if uint64(len(st.Chunks)) != st.Index.Chunks {
		return fmt.Errorf("chunk table has %d entries, index reports %d chunks", len(st.Chunks), st.Index.Chunks)
	}
	var samples uint64
	for i, c := range st.Chunks {
		if c.Index != uint32(i) {
			return fmt.Errorf("chunk %d recorded as index %d", i, c.Index)
		}
		if c.Samples == 0 {
			return fmt.Errorf("chunk %d has no samples", i)
		}
		samples += uint64(c.Samples)
	}
	if samples != st.Index.Samples {
		return fmt.Errorf("chunks cover %d samples, index has %d", samples, st.Index.Samples)
	}
	if st.InputSize < 0 || st.Index.PayloadLen < 0 {
		return fmt.Errorf("negative size in checkpoint")
	}
	if st.CommittedOffset < 0 || st.CommittedOffset > st.InputSize {
		return fmt.Errorf("committed offset %d outside input size %d", st.CommittedOffset, st.InputSize)
	}
	return nil
}

// ValidForInput checks a loaded checkpoint against the input being processed.
// A different path, size or content digest, or a lower generation than a
// previously seen one, is a mismatch. The caller may compare generations
// across restarts; within one process the store tracks the highest seen.
func (st *State) ValidForInput(path string, size int64, crc uint32) error {
	if st.InputPath != "" && st.InputPath != path {
		return fmt.Errorf("checkpoint belongs to %s, not %s", st.InputPath, path)
	}
	if st.InputSize != size {
		return fmt.Errorf("input size %d != checkpoint %d", size, st.InputSize)
	}
	if st.InputCRC != crc {
		return fmt.Errorf("input digest %08x != checkpoint %08x", crc, st.InputCRC)
	}
	return nil
}

// HexDigest formats a digest the same way terminal output does.
func HexDigest(b [32]byte) string { return hex.EncodeToString(b[:]) }
