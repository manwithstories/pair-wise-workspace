package txn

import (
	"bufio"
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

	"mediacheck/internal/media"
)

const walRelPath = ".mediacheck/wal.jsonl"

// Record is one append-only WAL entry.
type Record struct {
	TxID          string `json:"tx_id"`
	Stage         string `json:"stage"`
	Status        string `json:"status"`
	FinalRel      string `json:"final_rel"`
	TempRel       string `json:"temp_rel,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Size          int64  `json:"size,omitempty"`
	DurationTicks int64  `json:"duration_ticks,omitempty"`
	Offset        int64  `json:"offset,omitempty"`
	PTS           int64  `json:"pts,omitempty"`
}

// WAL is a JSON-lines write-ahead log inside the permitted output root.
type WAL struct {
	root string
	path string
	txID string
	mu   sync.Mutex
	file *os.File
}

// SafeJoin confines name to root and rejects absolute paths and traversal.
func SafeJoin(root, name string) (string, error) {
	if name == "" {
		return "", media.E(media.StageWAL, media.KindPathTraversal, -1, errors.New("empty path"))
	}
	if filepath.IsAbs(name) {
		return "", media.E(media.StageWAL, media.KindPathTraversal, -1,
			fmt.Errorf("absolute path %q is not allowed", name))
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", media.E(media.StageWAL, media.KindResource, -1, err)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", media.E(media.StageWAL, media.KindPathTraversal, -1,
			fmt.Errorf("path %q escapes output root", name))
	}
	target := filepath.Join(rootAbs, clean)
	rel, err := filepath.Rel(rootAbs, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", media.E(media.StageWAL, media.KindPathTraversal, -1,
			fmt.Errorf("path %q escapes output root", name))
	}
	return target, nil
}

// Open creates the output root and opens/fsyncs the WAL.
func Open(root, txID string) (*WAL, error) {
	walPath, err := SafeJoin(root, walRelPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(walPath), 0o755); err != nil {
		return nil, media.E(media.StageWAL, media.KindResource, -1, err)
	}
	f, err := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, media.E(media.StageWAL, media.KindResource, -1, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, media.E(media.StageWAL, media.KindResource, -1, err)
	}
	return &WAL{root: root, path: walPath, txID: txID, file: f}, nil
}

// Close syncs and closes the WAL.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *WAL) append(rec Record) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if rec.TxID == "" {
		rec.TxID = w.txID
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return rec, media.E(media.StageWAL, media.KindInvalidArgument, rec.Offset, err)
	}
	b = append(b, '\n')
	if _, err := w.file.Write(b); err != nil {
		return rec, media.E(media.StageWAL, media.KindResource, rec.Offset, err)
	}
	if err := w.file.Sync(); err != nil {
		return rec, media.E(media.StageWAL, media.KindResource, rec.Offset, err)
	}
	return rec, nil
}

// ReadRecords reads all WAL records.
func ReadRecords(root string) ([]Record, error) {
	path, err := SafeJoin(root, walRelPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, media.E(media.StageRecover, media.KindResource, -1, err)
	}
	defer f.Close()
	var out []Record
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for s.Scan() {
		var rec Record
		if err := json.Unmarshal(s.Bytes(), &rec); err != nil {
			return out, media.E(media.StageRecover, media.KindValidation, -1, err)
		}
		out = append(out, rec)
	}
	if err := s.Err(); err != nil {
		return out, media.E(media.StageRecover, media.KindResource, -1, err)
	}
	return out, nil
}

// PendingStage tracks a temp artifact between WAL begin and atomic commit.
type PendingStage struct {
	WAL       *WAL
	Record    Record
	tempPath  string
	finalPath string
}

// Begin writes the first WAL record and reserves a deterministic temp file.
func (w *WAL) Begin(stage, finalRel string, durationTicks int64) (*PendingStage, error) {
	tempRel := finalRel + ".tmp"
	tempPath, err := SafeJoin(w.root, tempRel)
	if err != nil {
		return nil, err
	}
	finalPath, err := SafeJoin(w.root, finalRel)
	if err != nil {
		return nil, err
	}
	rec := Record{Stage: stage, Status: "began", FinalRel: finalRel,
		TempRel: tempRel, DurationTicks: durationTicks, Offset: -1}
	if _, err := w.append(rec); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		return nil, media.E(media.StageWAL, media.KindResource, -1, err)
	}
	return &PendingStage{WAL: w, Record: rec, tempPath: tempPath, finalPath: finalPath}, nil
}

// Write writes and hashes the temporary artifact.
func (p *PendingStage) Write(data []byte) error {
	sum := sha256.Sum256(data)
	p.Record.SHA256 = hex.EncodeToString(sum[:])
	p.Record.Size = int64(len(data))
	if err := os.WriteFile(p.tempPath, data, 0o644); err != nil {
		return media.E(media.StageWAL, media.KindResource, p.Record.Offset, err)
	}
	f, err := os.Open(p.tempPath)
	if err != nil {
		return media.E(media.StageWAL, media.KindResource, p.Record.Offset, err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return media.E(media.StageWAL, media.KindResource, p.Record.Offset, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return media.E(media.StageWAL, media.KindResource, p.Record.Offset, err)
	}
	if err := f.Close(); err != nil {
		return media.E(media.StageWAL, media.KindResource, p.Record.Offset, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != p.Record.SHA256 {
		return media.E(media.StageWAL, media.KindHashMismatch, 0,
			fmt.Errorf("temp hash changed before prepare"))
	}
	p.Record.Status = "prepared"
	_, err = p.WAL.append(p.Record)
	return err
}

// Verify runs caller-supplied duration/index/content checks before commit.
func (p *PendingStage) Verify(check func(Record) error) error {
	if check != nil {
		if err := check(p.Record); err != nil {
			return err
		}
	}
	p.Record.Status = "verified"
	_, err := p.WAL.append(p.Record)
	return err
}

// Commit atomically renames the temp artifact and records the committed hash.
func (p *PendingStage) Commit() (Record, error) {
	if err := os.Rename(p.tempPath, p.finalPath); err != nil {
		return p.Record, media.E(media.StageWAL, media.KindResource, p.Record.Offset, err)
	}
	if d, err := os.Open(filepath.Dir(p.finalPath)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	p.Record.Status = "committed"
	return p.WAL.append(p.Record)
}

// SHA256Bytes returns the lowercase hex SHA-256 of data.
func SHA256Bytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
