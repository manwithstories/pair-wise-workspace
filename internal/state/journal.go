package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"turncode/internal/quality"
	"turncode/internal/rate"
)

const (
	JournalVersion = 1
	stateFile      = "state.json"
	stateBackup    = "state.json.bak"
	checkpointDir  = "checkpoints"
	latestCP       = "checkpoint.json"
	maxJournalSize = 8 << 20
)

var (
	ErrCorruptJournal = errors.New("corrupt journal")
	ErrNoCheckpoint   = errors.New("no usable checkpoint")
)

// StateRecord is protected by version and a checksum over all other fields.
type StateRecord struct {
	Version  int    `json:"version"`
	State    State  `json:"state"`
	Sequence uint64 `json:"sequence"`
	CRC      string `json:"crc"`
}

type StateStore struct {
	dir string
	mu  sync.Mutex
}

func NewStateStore(dir string) *StateStore { return &StateStore{dir: dir} }

// Save writes state atomically and retains the prior valid record as a backup.
func (s *StateStore) Save(current State) (StateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, stateFile)
	rec := StateRecord{Version: JournalVersion, State: current}
	if old, err := loadStateFile(path); err == nil {
		rec.Sequence = old.Sequence + 1
		oldRaw, readErr := os.ReadFile(path)
		if readErr == nil {
			if err := AtomicWrite(filepath.Join(s.dir, stateBackup), oldRaw, 0o600); err != nil {
				return rec, err
			}
		}
	}
	raw, err := sealState(rec)
	if err != nil {
		return rec, err
	}
	if err := AtomicWrite(path, raw, 0o600); err != nil {
		return rec, err
	}
	return rec, nil
}

// Load returns the primary record, or a valid backup with recovered=true.
func (s *StateStore) Load() (StateRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, stateFile)
	rec, err := loadStateFile(path)
	if err == nil {
		return rec, false, nil
	}
	backup, backupErr := loadStateFile(filepath.Join(s.dir, stateBackup))
	if backupErr == nil {
		return backup, true, nil
	}
	return StateRecord{}, false, fmt.Errorf("%w: primary: %v; backup: %v", ErrCorruptJournal, err, backupErr)
}

func sealState(rec StateRecord) ([]byte, error) {
	rec.Version = JournalVersion
	rec.CRC = ""
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	rec.CRC = hex.EncodeToString(sum[:])
	return json.MarshalIndent(rec, "", "  ")
}

func loadStateFile(path string) (StateRecord, error) {
	if err := rejectSymlink(path); err != nil {
		return StateRecord{}, err
	}
	raw, err := readLimited(path, 1<<20)
	if err != nil {
		return StateRecord{}, err
	}
	var rec StateRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return StateRecord{}, fmt.Errorf("%w: %v", ErrCorruptJournal, err)
	}
	if rec.Version != JournalVersion || !validState(rec.State) {
		return StateRecord{}, fmt.Errorf("%w: state version/name", ErrCorruptJournal)
	}
	want, err := sealState(rec)
	if err != nil {
		return StateRecord{}, err
	}
	var sealed StateRecord
	if err := json.Unmarshal(want, &sealed); err != nil || sealed.CRC != rec.CRC {
		return StateRecord{}, fmt.Errorf("%w: state checksum", ErrCorruptJournal)
	}
	return rec, nil
}

func validState(s State) bool {
	switch s {
	case Pending, Validating, Analyzing, Encoding, Verifying, Done, Paused, Failed, Cancelled:
		return true
	default:
		return false
	}
}

// AtomicWrite writes via a sibling temporary file, fsyncs it, renames it over
// the destination, then fsyncs the directory.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".journal-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: file too large", ErrCorruptJournal)
	}
	return data, nil
}

func checkpointName(gop int) string {
	return fmt.Sprintf("gop-%06d.json", gop)
}

func parseCheckpointName(name string) (int, bool) {
	if !strings.HasPrefix(name, "gop-") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	var gop int
	if _, err := fmt.Sscanf(name, "gop-%06d.json", &gop); err != nil {
		return 0, false
	}
	return gop, true
}

// Checkpoint captures the first frame and exact byte offset of the next GOP.
type Checkpoint struct {
	Version         int                 `json:"version"`
	SpecHash        string              `json:"spec_hash"`
	CompletedGOPs   int                 `json:"completed_gops"`
	NextFrame       int                 `json:"next_frame"`
	BitstreamOffset int64               `json:"bitstream_offset"`
	QP              int                 `json:"qp"`
	Buffer          rate.BufferState    `json:"buffer"`
	Quality         quality.Accumulator `json:"quality"`
	ResumedFromGOP  int                 `json:"resumed_from_gop"`
	LastGOPBytes    int64               `json:"last_gop_bytes"`
	LastGOPFrames   int                 `json:"last_gop_frames"`
	CRC             string              `json:"crc"`
}

// CheckpointStore writes one full record per GOP plus a latest pointer.
type CheckpointStore struct {
	dir string
}

func NewCheckpointStore(workDir string) *CheckpointStore {
	return &CheckpointStore{dir: filepath.Join(workDir, checkpointDir)}
}

// SaveCheckpoint is idempotent for an already persisted identical GOP.
func (s *CheckpointStore) SaveCheckpoint(cp Checkpoint) error {
	cp.Version = JournalVersion
	if cp.CompletedGOPs < 0 || cp.BitstreamOffset < 0 || len(cp.Quality.Segments) > 100000 {
		return fmt.Errorf("%w: invalid checkpoint fields", ErrCorruptJournal)
	}
	raw, err := sealCheckpoint(cp)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, checkpointName(cp.CompletedGOPs))
	if old, err := loadCheckpointFile(path); err == nil {
		if old.BitstreamOffset == cp.BitstreamOffset && old.NextFrame == cp.NextFrame && old.CompletedGOPs == cp.CompletedGOPs {
			return nil
		}
	}
	if err := AtomicWrite(path, raw, 0o600); err != nil {
		return err
	}
	// checkpoint.json is a durable latest pointer, not the source of replay.
	return AtomicWrite(filepath.Join(s.dir, latestCP), raw, 0o600)
}

func sealCheckpoint(cp Checkpoint) ([]byte, error) {
	cp.Version = JournalVersion
	cp.CRC = ""
	raw, err := json.Marshal(cp)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	cp.CRC = hex.EncodeToString(sum[:])
	return json.MarshalIndent(cp, "", "  ")
}

func loadCheckpointFile(path string) (Checkpoint, error) {
	if err := rejectSymlink(path); err != nil {
		return Checkpoint{}, err
	}
	raw, err := readLimited(path, maxJournalSize)
	if err != nil {
		return Checkpoint{}, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return Checkpoint{}, fmt.Errorf("%w: %v", ErrCorruptJournal, err)
	}
	if cp.Version != JournalVersion || cp.CompletedGOPs < 0 || cp.BitstreamOffset < 0 || cp.QP < 0 || cp.QP > 51 {
		return Checkpoint{}, fmt.Errorf("%w: checkpoint fields", ErrCorruptJournal)
	}
	want, err := sealCheckpoint(cp)
	if err != nil {
		return Checkpoint{}, err
	}
	var sealed Checkpoint
	if err := json.Unmarshal(want, &sealed); err != nil || sealed.CRC != cp.CRC {
		return Checkpoint{}, fmt.Errorf("%w: checkpoint checksum", ErrCorruptJournal)
	}
	return cp, nil
}

// Recovery describes the furthest durable GOP whose bytes are still present.
type Recovery struct {
	Checkpoint    Checkpoint
	Fallback      bool
	Truncate      bool
	BitstreamSize int64
}

// Recover scans the per-GOP journal and chooses the highest valid, complete
// checkpoint that does not extend past the current bitstream. A longer
// bitstream is rolled back to that checkpoint offset.
func (s *CheckpointStore) Recover(bitstreamSize int64, specHash string) (Recovery, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Recovery{}, ErrNoCheckpoint
		}
		return Recovery{}, err
	}
	var best Checkpoint
	found := false
	highestValid := -1
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		gop, ok := parseCheckpointName(entry.Name())
		if !ok {
			continue
		}
		cp, err := loadCheckpointFile(filepath.Join(s.dir, entry.Name()))
		if err != nil || cp.CompletedGOPs != gop {
			continue
		}
		if specHash != "" && cp.SpecHash != "" && cp.SpecHash != specHash {
			continue
		}
		if gop > highestValid {
			highestValid = gop
		}
		if cp.BitstreamOffset <= bitstreamSize && (!found || cp.CompletedGOPs > best.CompletedGOPs) {
			best, found = cp, true
		}
	}
	if !found {
		return Recovery{}, ErrNoCheckpoint
	}
	return Recovery{
		Checkpoint:    best,
		Fallback:      best.CompletedGOPs < highestValid,
		Truncate:      bitstreamSize > best.BitstreamOffset,
		BitstreamSize: bitstreamSize,
	}, nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: symlink rejected", ErrCorruptJournal)
	}
	return nil
}

// Latest loads checkpoint.json only if it is valid. It is useful for reports
// after a clean completion; replay should use Recover.
func (s *CheckpointStore) Latest() (Checkpoint, error) {
	return loadCheckpointFile(filepath.Join(s.dir, latestCP))
}
