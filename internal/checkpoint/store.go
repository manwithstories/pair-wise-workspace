// Package checkpoint persists only fully validated processing boundaries so a
// cancelled or crashed run resumes deterministically. A checkpoint is written
// to a temp file and atomically renamed; resumption first re-hashes the input
// and refuses to continue if the fingerprint changed.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"mediaarchive/internal/model"
)

// State is the on-disk checkpoint document.
type State struct {
	Version     int    `json:"version"`
	InputPath   string `json:"input_path"`
	InputSize   int64  `json:"input_size"`
	InputSHA256 string `json:"input_sha256"`
	// CompletedChunks are chunk seq numbers fully validated and written.
	CompletedChunks []uint32 `json:"completed_chunks"`
	// LastOrder is the highest global job order committed.
	LastOrder uint64 `json:"last_order"`
	// SamplesCommitted counts packets already in the output.
	SamplesCommitted uint64 `json:"samples_committed"`
}

// Fingerprint hashes an input file in streaming fashion.
func Fingerprint(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, model.NewError(model.StageCheckpt, "cannot open input for fingerprint").Wrap(err)
	}
	defer f.Close()
	h := sha256.New()
	size, err := copyN(f, h)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// Load reads and verifies a checkpoint. A missing file returns (nil, nil).
// A fingerprint mismatch is an unrecoverable checkpoint error: the input is
// not the one the checkpoint was built against.
func Load(path, inputPath, inputSHA string, inputSize int64) (*State, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, model.NewError(model.StageCheckpt, "cannot read checkpoint").Wrap(err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, model.NewError(model.StageCheckpt, "corrupt checkpoint json").Wrap(err)
	}
	if st.InputPath != inputPath || st.InputSHA256 != inputSHA || st.InputSize != inputSize {
		return nil, model.NewError(model.StageCheckpt,
			"checkpoint input fingerprint mismatch; refusing to resume")
	}
	return &st, nil
}

// IsChunkDone reports whether a whole chunk boundary was already committed,
// making its reprocessing idempotent.
func (s *State) IsChunkDone(seq uint32) bool {
	for _, c := range s.CompletedChunks {
		if c == seq {
			return true
		}
	}
	return false
}

// Save atomically replaces the checkpoint file.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return model.NewError(model.StageCheckpt, "cannot create checkpoint dir").Wrap(err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return model.NewError(model.StageCheckpt, "cannot encode checkpoint").Wrap(err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".chk-*")
	if err != nil {
		return model.NewError(model.StageCheckpt, "cannot create temp checkpoint").Wrap(err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return model.NewError(model.StageCheckpt, "cannot write checkpoint").Wrap(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return model.NewError(model.StageCheckpt, "checkpoint fsync failed").Wrap(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return model.NewError(model.StageCheckpt, "checkpoint close failed").Wrap(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return model.NewError(model.StageCheckpt, "checkpoint atomic rename failed").Wrap(err)
	}
	return nil
}

// MarkChunk records one fully validated and written chunk boundary.
func (s *State) MarkChunk(seq uint32, lastOrder, samples uint64) {
	if !s.IsChunkDone(seq) {
		s.CompletedChunks = append(s.CompletedChunks, seq)
	}
	if lastOrder > s.LastOrder {
		s.LastOrder = lastOrder
	}
	s.SamplesCommitted = samples
}

func copyN(r io.Reader, w io.Writer) (int64, error) {
	n, err := io.Copy(w, r)
	if err != nil {
		return 0, model.NewError(model.StageCheckpt, "input fingerprint read failed").Wrap(err)
	}
	return n, nil
}
