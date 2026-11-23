// Package checkpoint persists pipeline progress so an interrupted run can
// resume without redoing completed work.
//
// A checkpoint is committed only after the artifacts it references are on
// disk: the frames appended so far, the index fragment and the manifest
// digest. Committing before those exist would let a restart trust progress
// that never became durable, so Ordering below makes the dependency explicit
// and State.Verify refuses a checkpoint whose artifacts do not match.
package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mediad/internal/clock"
)

// Magic and Version guard against reading a checkpoint from another format.
const (
	Magic   = "MDCKPT1"
	Version = 1
)

// Stage names the pipeline phases whose completion a checkpoint records.
type Stage string

const (
	// StageIngest means every frame up to NextFrame has been parsed and
	// appended to the frames file.
	StageIngest Stage = "ingest"
	// StageIndex means the index fragment covering [0, IndexedFrames) is on
	// disk.
	StageIndex Stage = "index"
	// StageManifest means the manifest digest for the framed range has been
	// computed and stored.
	StageManifest Stage = "manifest"
)

// Artifact is a durable file a checkpoint depends on. Digest binds the
// checkpoint to the exact bytes that were on disk when it committed.
type Artifact struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// State is the serialized checkpoint. It is small and fixed-size: it records
// stage progress and O(1) mapper/sequencer state, never frame contents.
type State struct {
	Magic       string         `json:"magic"`
	Version     int            `json:"version"`
	TrackID     uint32         `json:"track_id"`
	NextFrame   uint64         `json:"next_frame"` // frames durably ingested
	Indexed     uint64         `json:"indexed"`    // usable frames in the index fragment
	Stage       Stage          `json:"stage"`
	FramesFile  Artifact       `json:"frames_file"`
	IndexFile   Artifact       `json:"index_file"`
	ManifestSum string         `json:"manifest_digest"`
	Mapper      clock.State    `json:"mapper"`
	Sequencer   clock.SeqState `json:"sequencer"`
	Updated     time.Time      `json:"updated"`
}

// ErrUnfinished reports a checkpoint referencing artifacts that are missing or
// changed. The pipeline must restart the affected stage rather than trust it.
var ErrUnfinished = errors.New("checkpoint: referenced artifact missing or changed")

// ErrNoCheckpoint reports that no committed checkpoint exists.
var ErrNoCheckpoint = errors.New("checkpoint: none")

// Ordering documents and enforces the commit rule: frames, then index, then
// manifest. Save refuses a stage whose prerequisites are absent.
func Ordering(st State) error {
	switch st.Stage {
	case StageIngest:
		return nil
	case StageIndex:
		if st.FramesFile.Digest == "" {
			return errors.New("checkpoint: index stage without durable frames")
		}
	case StageManifest:
		if st.FramesFile.Digest == "" || st.IndexFile.Digest == "" {
			return errors.New("checkpoint: manifest stage without durable frames and index")
		}
		if st.ManifestSum == "" {
			return errors.New("checkpoint: manifest stage without a manifest digest")
		}
	default:
		return fmt.Errorf("checkpoint: unknown stage %q", st.Stage)
	}
	return nil
}

// Save validates the stage ordering, stamps the state and writes it to path by
// atomic rename. It refuses to overwrite an existing checkpoint with an older
// one (NextFrame must not move backwards) so a resumed run cannot regress.
func Save(path string, st State) error {
	if err := Ordering(st); err != nil {
		return err
	}
	st.Magic = Magic
	st.Version = Version
	st.Updated = time.Now().UTC()
	if prev, err := Load(path); err == nil && st.NextFrame < prev.NextFrame {
		return fmt.Errorf("checkpoint: refusing to regress next_frame %d -> %d", prev.NextFrame, st.NextFrame)
	}
	return writeAtomic(path, st)
}

func writeAtomic(path string, st State) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ckpt-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	ok = true
	return os.Rename(name, path)
}

// Load reads and structurally validates a checkpoint.
func Load(path string) (State, error) {
	var st State
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, ErrNoCheckpoint
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("checkpoint: corrupt state: %w", err)
	}
	if st.Magic != Magic || st.Version != Version {
		return st, fmt.Errorf("checkpoint: wrong magic/version %q/%d", st.Magic, st.Version)
	}
	if err := Ordering(st); err != nil {
		return st, err
	}
	return st, nil
}

// Verify re-hashes every artifact the checkpoint references and compares it to
// the recorded digest. A run may resume only when this returns nil.
func Verify(st State) error {
	if err := checkArtifact(st.FramesFile); err != nil {
		return fmt.Errorf("%w: frames: %v", ErrUnfinished, err)
	}
	if st.Stage == StageIndex || st.Stage == StageManifest {
		if err := checkArtifact(st.IndexFile); err != nil {
			return fmt.Errorf("%w: index: %v", ErrUnfinished, err)
		}
	}
	return nil
}

func checkArtifact(a Artifact) error {
	if a.Path == "" || a.Digest == "" {
		return errors.New("empty artifact reference")
	}
	fi, err := os.Stat(a.Path)
	if err != nil {
		return err
	}
	if a.Size != 0 && fi.Size() != a.Size {
		return fmt.Errorf("size %d != %d", fi.Size(), a.Size)
	}
	d, err := FileDigest(a.Path)
	if err != nil {
		return err
	}
	if d != a.Digest {
		return fmt.Errorf("digest %s != %s", d, a.Digest)
	}
	return nil
}

// FileDigest returns the hex SHA-256 of a file's contents.
func FileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// ArtifactOf describes a durable file for a checkpoint.
func ArtifactOf(path string) (Artifact, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Artifact{}, err
	}
	d, err := FileDigest(path)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Path: path, Size: fi.Size(), Digest: d}, nil
}

// Clear removes a checkpoint. It is used when inputs changed and no prior
// progress may be trusted.
func Clear(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
