// Package checkpoint persists resumable job state. A checkpoint records the
// exact byte offset and SHA-256 state of the partial output, so an interrupted
// write can continue and end with the same digest as an uninterrupted run.
package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediapipe/internal/media"
	"mediapipe/internal/remux"
)

// Version is bumped when the on-disk schema changes.
const Version = 1

// State is one persisted checkpoint.
type State struct {
	Version   int             `json:"version"`
	JobID     string          `json:"job_id"`
	Output    string          `json:"output"`
	Target    string          `json:"target"`
	Bytes     int64           `json:"bytes"`
	Packets   int             `json:"packets"`
	Clusters  int             `json:"clusters"`
	HashState []byte          `json:"hash_state"`
	TimeStart media.Timestamp `json:"time_start_ns"`
	TimeEnd   media.Timestamp `json:"time_end_ns"`
	HaveTime  bool            `json:"have_time"`
	// Incremental digest chains the checkpoint's own fields so accidental
	// edits are detected on load.
	Checksum string `json:"checksum"`
}

// Store writes and reads checkpoints under a directory.
type Store struct {
	Dir string
}

// NewStore returns a store rooted at dir, creating it if needed.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, media.NewError("checkpoint.store", media.ClassFatal, fmt.Errorf("empty checkpoint directory"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, media.NewError("checkpoint.store", media.ClassInterrupted, err)
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) path(jobID string) string {
	return filepath.Join(s.Dir, jobID+".ckpt.json")
}

// Save persists a progress snapshot. The write is atomic: a crash leaves
// either the previous checkpoint or the new one, never a torn file.
func (s *Store) Save(jobID, output, target string, p remux.Progress) error {
	st := State{
		Version:   Version,
		JobID:     jobID,
		Output:    output,
		Target:    target,
		Bytes:     p.Bytes,
		Packets:   p.Packets,
		Clusters:  p.Clusters,
		HashState: p.HashState,
		TimeStart: p.TimeStart,
		TimeEnd:   p.TimeEnd,
		HaveTime:  p.HaveTime,
	}
	st.Checksum = checksum(st)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return media.NewError("checkpoint.save", media.ClassInterrupted, err)
	}
	tmp := s.path(jobID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return media.NewError("checkpoint.save", media.ClassInterrupted, err)
	}
	if err := os.Rename(tmp, s.path(jobID)); err != nil {
		return media.NewError("checkpoint.save", media.ClassInterrupted, err)
	}
	return nil
}

// Load reads a checkpoint for jobID. A missing checkpoint is not an error;
// callers get (nil, nil) and start from scratch.
func (s *Store) Load(jobID string) (*State, error) {
	data, err := os.ReadFile(s.path(jobID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, media.NewError("checkpoint.load", media.ClassInterrupted, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, media.NewError("checkpoint.load", media.ClassFatal, err)
	}
	if st.Version != Version {
		return nil, media.NewError("checkpoint.load", media.ClassUnsupported,
			fmt.Errorf("checkpoint version %d, want %d", st.Version, Version))
	}
	if st.Checksum != checksum(st) {
		return nil, media.NewError("checkpoint.load", media.ClassFatal,
			fmt.Errorf("checkpoint checksum mismatch for job %q", jobID))
	}
	return &st, nil
}

// Resume converts a checkpoint into remux resume options.
func (st *State) Resume() *remux.ResumeState {
	return &remux.ResumeState{
		Progress: remux.Progress{
			Bytes: st.Bytes, Packets: st.Packets, Clusters: st.Clusters,
			HashState: st.HashState, TimeStart: st.TimeStart,
			TimeEnd: st.TimeEnd, HaveTime: st.HaveTime,
		},
		TruncateTo: st.Bytes,
	}
}

// TruncateOutput cuts the output file back to the checkpointed byte length so
// resumed writing continues from exactly that offset.
func (st *State) TruncateOutput(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return media.NewError("checkpoint.truncate", media.ClassInterrupted, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return media.NewError("checkpoint.truncate", media.ClassInterrupted, err)
	}
	if info.Size() < st.Bytes {
		return media.NewError("checkpoint.truncate", media.ClassFatal,
			fmt.Errorf("output is %d bytes, checkpoint expects at least %d", info.Size(), st.Bytes))
	}
	if err := f.Truncate(st.Bytes); err != nil {
		return media.NewError("checkpoint.truncate", media.ClassInterrupted, err)
	}
	return nil
}

// Delete removes a checkpoint after a successful commit.
func (s *Store) Delete(jobID string) error {
	if err := os.Remove(s.path(jobID)); err != nil && !os.IsNotExist(err) {
		return media.NewError("checkpoint.delete", media.ClassInterrupted, err)
	}
	return nil
}

// checksum hashes the state's identifying and progress fields.
func checksum(st State) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%s|%s|%d|%d|%d|%d|%d|%v", st.Version, st.JobID, st.Output,
		st.Target, st.Bytes, st.Packets, st.Clusters, st.TimeStart, st.TimeEnd, st.HaveTime)
	h.Write(st.HashState)
	return fmt.Sprintf("%x", h.Sum(nil))
}
