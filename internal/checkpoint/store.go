// Package checkpoint persists pipeline progress atomically. A checkpoint is
// only valid when the input digest, the source identity and every completed
// index block digest still match; half-written or torn files are refused.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"archive.local/archive/internal/model"
	"archive.local/archive/internal/repair"
)

// BlockID names one completed index range: a track and entry indices.
type BlockID struct {
	TrackID uint32 `json:"track_id"`
	From    int    `json:"from"`
	To      int    `json:"to"`
}

// Block records a completed index block and the fingerprint its content had
// when it was written.
type Block struct {
	ID          BlockID `json:"id"`
	Fingerprint string  `json:"fingerprint"`
}

// State is the resumable pipeline state.
type State struct {
	InputDigest   string  `json:"input_digest"`
	SourceID      string  `json:"source_id"`
	ConfirmedHigh int64   `json:"confirmed_high"`
	Completed     []Block `json:"completed"`
	IndexDigest   string  `json:"index_digest,omitempty"`
	NextTrack     uint32  `json:"next_track"`
	NextEntry     int     `json:"next_entry"`
}

// file is the on-disk envelope: the state plus a checksum over its canonical
// bytes, so a torn write is detectable even when the JSON happens to parse.
type file struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	State    json.RawMessage `json:"state"`
}

const version = 2

// Store writes checkpoints under an explicit directory.
type Store struct {
	Dir string
}

// NewStore creates the checkpoint directory with owner-only permissions.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) path() string { return filepath.Join(s.Dir, "checkpoint.json") }

// Save writes st atomically: a private temp file, fsync, then rename over the
// live checkpoint. A crash can leave a .tmp file but never a partial live
// checkpoint.
func (s *Store) Save(st State) error {
	st.Completed = append([]Block(nil), st.Completed...)
	canonical, err := canonicalState(st)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	env := file{Version: version, Checksum: hex.EncodeToString(sum[:]), State: canonical}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, "checkpoint-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path()); err != nil {
		return err
	}
	// fsync the directory so the rename itself survives a crash.
	if d, err := os.Open(s.Dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Load reads and validates the latest checkpoint. It returns a nil state and
// a nil error when no checkpoint exists (a clean first run).
func (s *Store) Load() (*State, error) {
	body, err := os.ReadFile(s.path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var env file
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, invalid("checkpoint is not valid json")
	}
	if env.Version != version {
		return nil, invalid(fmt.Sprintf("checkpoint version %d is not %d", env.Version, version))
	}
	var st State
	if err := json.Unmarshal(env.State, &st); err != nil {
		return nil, invalid("checkpoint state is not valid json")
	}
	canonical, err := canonicalState(st)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	if got := hex.EncodeToString(sum[:]); got != env.Checksum {
		return nil, invalid("checkpoint checksum does not match (half-written file)")
	}
	return &st, nil
}

// Validate re-checks a loaded state against the current input. The resume is
// only allowed when the input digest, the source identity and every completed
// block fingerprint still match the freshly built index.
func Validate(st *State, inputDigest, sourceID string, idx *repair.Index) error {
	if st.InputDigest != inputDigest {
		return mismatch("input digest changed since the checkpoint")
	}
	if st.SourceID != sourceID {
		return mismatch("source identity changed since the checkpoint")
	}
	blocks := make(map[BlockID]string, len(st.Completed))
	for _, b := range st.Completed {
		blocks[b.ID] = b.Fingerprint
	}
	if len(blocks) == 0 {
		return nil
	}
	for i := range idx.Tracks {
		ti := &idx.Tracks[i]
		for id, want := range blocks {
			if id.TrackID != ti.TrackID {
				continue
			}
			got, err := ti.BlockFingerprint(id.From, id.To)
			if err != nil {
				return mismatch(fmt.Sprintf("completed block %+v no longer exists", id))
			}
			if got != want {
				return mismatch(fmt.Sprintf("index block %+v changed since the checkpoint", id))
			}
		}
	}
	return nil
}

// MarkCompleted appends a completed block unless it is already recorded.
func (st *State) MarkCompleted(id BlockID, fp string) {
	for _, b := range st.Completed {
		if b.ID == id {
			return
		}
	}
	st.Completed = append(st.Completed, Block{ID: id, Fingerprint: fp})
}

// CompletedFingerprint returns the stored fingerprint for a block.
func (st *State) CompletedFingerprint(id BlockID) (string, bool) {
	for _, b := range st.Completed {
		if b.ID == id {
			return b.Fingerprint, true
		}
	}
	return "", false
}

func canonicalState(st State) ([]byte, error) {
	// json.Marshal sorts map keys but for structs it emits fields in
	// declaration order, which is what makes the checksum stable.
	tmp := st
	tmp.Completed = append([]Block(nil), st.Completed...)
	sortBlocks(tmp.Completed)
	return json.Marshal(tmp)
}

func sortBlocks(blocks []Block) {
	for i := 1; i < len(blocks); i++ {
		v := blocks[i]
		j := i - 1
		for j >= 0 && less(v, blocks[j]) {
			blocks[j+1] = blocks[j]
			j--
		}
		blocks[j+1] = v
	}
}

func less(a, b Block) bool {
	if a.ID.TrackID != b.ID.TrackID {
		return a.ID.TrackID < b.ID.TrackID
	}
	if a.ID.From != b.ID.From {
		return a.ID.From < b.ID.From
	}
	return a.ID.To < b.ID.To
}

func invalid(detail string) *model.Error {
	return &model.Error{Code: model.CodeCheckpointInvalid, Sample: -1, OffsetBytes: -1, Detail: detail}
}

func mismatch(detail string) *model.Error {
	return &model.Error{Code: model.CodeCheckpointMismatch, Sample: -1, OffsetBytes: -1, Detail: detail}
}
