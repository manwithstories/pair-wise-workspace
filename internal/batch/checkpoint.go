// Package batch coordinates interruption and recovery for one archive run.
// It derives stable digests of the input capture and the processing
// parameters, persists a run-state checkpoint, and guarantees that on
// cancellation or write failure the already-committed index chunks and
// segments are left intact for a later resume rather than being rolled
// back or half-overwritten.
package batch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"archive/internal/model"
)

// Params is the full set of parameters whose identity must stay stable
// across a resume. Changing any of these changes the digest and forces a
// clean restart rather than a silent mis-resume.
type Params struct {
	Tracks        int   `json:"tracks"`
	Packets       int   `json:"packets"`
	ReorderWindow int   `json:"reorder_window"`
	ChunkPackets  int   `json:"chunk_packets"`
	Queue         int   `json:"queue"`
	SlowWriterMs  int   `json:"slow_writer_ms"`
	Seed          int64 `json:"seed"`
}

// State is the persisted checkpoint of a run.
type State struct {
	Version        int    `json:"version"`
	InputDigest    string `json:"input_digest"`
	ParamDigest    string `json:"param_digest"`
	CompleteChunks int    `json:"complete_chunks"`
	Segments       int    `json:"segments"`
	Frames         int64  `json:"frames"`
	Finished       bool   `json:"finished"`
	Canceled       bool   `json:"canceled"`
}

const stateVersion = 1
const stateFile = "checkpoint.json"

// DigestInput returns the SHA-256 of the raw input datagrams.
func DigestInput(datagrams [][]byte) string {
	h := sha256.New()
	for _, d := range datagrams {
		var l [4]byte
		l[0] = byte(len(d) >> 24)
		l[1] = byte(len(d) >> 16)
		l[2] = byte(len(d) >> 8)
		l[3] = byte(len(d))
		h.Write(l[:])
		h.Write(d)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DigestParams returns the SHA-256 of the canonical parameter encoding.
func DigestParams(p Params) string {
	b, _ := json.Marshal(p) // deterministic for a fixed struct
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Save atomically persists run state, never deleting prior committed work.
func Save(dir string, st State) error {
	st.Version = stateVersion
	path := filepath.Join(dir, stateFile)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return model.New(model.KindIndex, path, "cannot encode checkpoint", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return model.New(model.KindIO, path, "cannot write checkpoint", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return model.New(model.KindIO, path, "cannot commit checkpoint", err)
	}
	return nil
}

// Load reads run state. A missing file returns (nil, nil).
func Load(dir string) (*State, error) {
	path := filepath.Join(dir, stateFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, model.New(model.KindIO, path, "cannot read checkpoint", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, model.New(model.KindIndex, path, "corrupt checkpoint json", err)
	}
	if st.Version != stateVersion {
		return nil, model.Newf(model.KindIndex, path, "checkpoint version %d unsupported", st.Version)
	}
	return &st, nil
}

// MarkCanceled flags the run as canceled while preserving committed chunks.
// It deliberately leaves every segment and index file in place.
func MarkCanceled(dir string, st State) error {
	st.Canceled = true
	st.Finished = false
	return Save(dir, st)
}

// MarkFinished records a cleanly completed run.
func MarkFinished(dir string, st State) error {
	st.Canceled = false
	st.Finished = true
	return Save(dir, st)
}
