// Package index persists deterministic, atomic processing checkpoints and
// validates them on resume. A torn or config-mismatched index is rejected so a
// corrupt tail can never resume over unvalidated chunks.
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediaaudit/internal/model"
)

// magic and version identify an on-disk index and its layout.
const (
	magic   = "PFI1"
	version = uint16(1)
	// headerSize: magic(4)+version(2)+payloadLen(8)+sha256(32) = 46 bytes.
	headerSize = 46
)

// Config fingerprints the inputs that must stay identical across resumes.
type Config struct {
	PackagePath       string `json:"package_path"`
	ChunkFrames       int64  `json:"chunk_frames"`
	CheckpointEvery   int64  `json:"checkpoint_every"`
	TileFrames        int64  `json:"tile_frames"`
	InputFingerprint  string `json:"input_fingerprint"`
}

// ChunkRecord is one fully verified, committed processing chunk.
type ChunkRecord struct {
	Seq          int64  `json:"seq"`
	FirstFrame   int64  `json:"first_frame"`
	LastFrame    int64  `json:"last_frame"`
	Status       string `json:"status"` // "ok" or "damaged"
	DamageCount  int    `json:"damage_count"`
}

// Payload is the versioned, JSON-encoded checkpoint body.
type Payload struct {
	Config        Config        `json:"config"`
	CommittedSeq  int64         `json:"committed_seq"` // highest fully committed chunk seq
	TotalFrames   int64         `json:"total_frames"`
	Chunks        []ChunkRecord `json:"chunks"`
	CheckpointNo  int64         `json:"checkpoint_no"`
	Finalized     bool          `json:"finalized"`
}

// File wraps a loaded index together with its raw checksum.
type File struct {
	Path    string
	Payload Payload
	sha     [32]byte // sha256 over the JSON payload
}

// SHA256 returns the hex checksum of the payload as stored.
func (f *File) SHA256() string { return hex.EncodeToString(f.sha[:]) }

// hashPayload serializes and hashes a payload deterministically.
func hashPayload(p Payload) ([]byte, [32]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return raw, sha256.Sum256(raw), nil
}

// writeAtomic serializes payload and replaces path via a temp file + rename,
// fsync-ing the directory so a crash cannot leave a half-written index.
func writeAtomic(path string, p Payload) error {
	raw, sum, err := hashPayload(p)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".pfi-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	hdr := make([]byte, headerSize)
	copy(hdr[0:4], magic)
	binary.LittleEndian.PutUint16(hdr[4:6], version)
	binary.LittleEndian.PutUint64(hdr[6:14], uint64(len(raw)))
	copy(hdr[14:46], sum[:])
	if _, err := tmp.Write(hdr); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Create writes a fresh index for cfg with no committed chunks.
func Create(path string, cfg Config, totalFrames int64) (*File, error) {
	p := Payload{
		Config:       cfg,
		TotalFrames:  totalFrames,
		Chunks:       []ChunkRecord{},
		CommittedSeq: -1, // no chunk committed yet; next seq is 0
	}
	if err := writeAtomic(path, p); err != nil {
		return nil, err
	}
	return Load(path)
}

// Load reads and fully validates an index: magic/version, length, checksum and
// JSON structure. Any structural defect returns a KindIndex fault.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < headerSize {
		return nil, indexFault("index shorter than header")
	}
	if string(b[0:4]) != magic {
		return nil, indexFault("bad magic; not a mediaaudit index")
	}
	if v := binary.LittleEndian.Uint16(b[4:6]); v != version {
		return nil, indexFault(fmt.Sprintf("unsupported index version %d", v))
	}
	plen := binary.LittleEndian.Uint64(b[6:14])
	if uint64(len(b)) != headerSize+plen {
		return nil, indexFault("index length mismatch (torn write)")
	}
	var stored [32]byte
	copy(stored[:], b[14:46])
	raw := b[headerSize:]
	if sha256.Sum256(raw) != stored {
		return nil, indexFault("index checksum mismatch (torn or corrupt)")
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, indexFault("index payload unmarshal: " + err.Error())
	}
	if err := validateInvariants(&p); err != nil {
		return nil, err
	}
	return &File{Path: path, Payload: p, sha: stored}, nil
}

// validateInvariants checks internal consistency independent of config.
func validateInvariants(p *Payload) error {
	if len(p.Chunks) == 0 {
		// A fresh index carries CommittedSeq -1; a finalized empty index is
		// only valid if there genuinely were zero frames.
		if p.CommittedSeq != -1 && !p.Finalized {
			return indexFault("committed seq set without chunk records")
		}
		return nil
	}
	// Chunks must be contiguous, ordered and match CommittedSeq.
	for i, c := range p.Chunks {
		if c.Seq != int64(i) {
			return indexFault(fmt.Sprintf("non-contiguous chunk seq at %d", i))
		}
		if c.LastFrame <= c.FirstFrame {
			return indexFault(fmt.Sprintf("chunk %d has empty frame range", i))
		}
	}
	last := p.Chunks[len(p.Chunks)-1]
	if last.Seq != p.CommittedSeq {
		return indexFault("committed seq does not match last chunk")
	}
	return nil
}

func indexFault(msg string) error {
	return model.NewFault(model.KindIndex, "", model.FrameRange{}, -1, msg)
}
