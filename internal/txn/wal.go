package txn

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

	"mediarig/internal/media"
)

const WALFileName = "wal.log"

// MaxParallelBatches is the process-wide concurrency policy. The CLI runs one
// root per invocation, while batch callers may acquire up to four slots.
const MaxParallelBatches = 4

// Artifact is a checksummed stage output referenced by a WAL record.
type Artifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Record is one checksummed write-ahead log entry.
type Record struct {
	Version      int        `json:"version"`
	Seq          int64      `json:"seq"`
	Stage        string     `json:"stage"`
	Status       string     `json:"status"`
	InputSHA256  string     `json:"input_sha256,omitempty"`
	ParamsSHA256 string     `json:"params_sha256,omitempty"`
	Artifacts    []Artifact `json:"artifacts,omitempty"`
	Message      string     `json:"message,omitempty"`
	Checksum     string     `json:"checksum"`
}

func (r Record) checksummed() (Record, error) {
	r.Checksum = ""
	b, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	sum := sha256.Sum256(b)
	r.Checksum = hex.EncodeToString(sum[:])
	return r, nil
}

func verifyRecord(line []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(line, &r); err != nil {
		return Record{}, err
	}
	want := r.Checksum
	calc, err := r.checksummed()
	if err != nil {
		return Record{}, err
	}
	if want == "" || want != calc.Checksum {
		return Record{}, fmt.Errorf("checksum mismatch")
	}
	return r, nil
}

// WAL is an append-only checksummed stage journal.
type WAL struct {
	path string
	f    *os.File
	seq  int64
}

func OpenWAL(path string) (*WAL, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, media.InternalWrap(media.StageWAL, err, "create WAL directory")
	}
	records, err := ReadAndRepair(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, media.InternalWrap(media.StageWAL, err, "open WAL for append")
	}
	var seq int64
	if len(records) > 0 {
		seq = records[len(records)-1].Seq
	}
	return &WAL{path: path, f: f, seq: seq}, nil
}

func (w *WAL) Close() error { return w.f.Close() }

// AppendCheckpoint writes and fsyncs one stage checkpoint record.
func (w *WAL) AppendCheckpoint(stage media.Stage, inputDigest, paramsDigest string, artifacts []Artifact, message string) (Record, error) {
	w.seq++
	r := Record{
		Version:      1,
		Seq:          w.seq,
		Stage:        string(stage),
		Status:       "checkpoint",
		InputSHA256:  inputDigest,
		ParamsSHA256: paramsDigest,
		Artifacts:    artifacts,
		Message:      message,
	}
	cr, err := r.checksummed()
	if err != nil {
		return Record{}, media.InternalWrap(media.StageWAL, err, "checksum WAL record")
	}
	line, err := json.Marshal(cr)
	if err != nil {
		return Record{}, media.InternalWrap(media.StageWAL, err, "encode WAL record")
	}
	line = append(line, '\n')
	if _, err := w.f.Write(line); err != nil {
		return Record{}, media.InternalWrap(media.StageWAL, err, "append WAL record")
	}
	if err := w.f.Sync(); err != nil {
		return Record{}, media.InternalWrap(media.StageWAL, err, "sync WAL record")
	}
	return cr, nil
}

// ReadAndRepair reads all valid WAL records. A torn final record is truncated;
// corruption in a record followed by another record is rejected.
func ReadAndRepair(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, media.InternalWrap(media.StageWAL, err, "read WAL")
	}
	var records []Record
	offset := 0
	for offset < len(data) {
		nl := bytesIndexByte(data[offset:], '\n')
		var line []byte
		lineStart := offset
		next := len(data)
		completeNewline := false
		if nl < 0 {
			line = data[offset:]
			offset = len(data)
		} else {
			line = data[offset : offset+nl]
			next = offset + nl + 1
			offset = next
			completeNewline = true
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		rec, verr := verifyRecord(line)
		if verr == nil {
			records = append(records, rec)
			continue
		}

		// Look for a later non-empty record. If one exists, this is middle
		// corruption and recovery must not truncate evidence.
		remaining := data[minInt(next, len(data)):]
		hasLater := len(strings.TrimSpace(string(remaining))) > 0
		if hasLater {
			return nil, media.Corrupt(media.StageRecover, "middle WAL record at byte %d is corrupt: %v", lineStart, verr)
		}
		if err := os.Truncate(path, int64(lineStart)); err != nil {
			return nil, media.InternalWrap(media.StageRecover, err, "truncate torn WAL tail")
		}
		if err := SyncDir(filepath.Dir(path)); err != nil {
			return nil, err
		}
		_ = completeNewline
		break
	}
	return records, nil
}

func bytesIndexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// WriteAtomic writes data in a same-directory temp file, fsyncs it, renames,
// and fsyncs the directory.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return media.InternalWrap(media.StageCommit, err, "create directory %s", dir)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return media.InternalWrap(media.StageCommit, err, "create temporary file")
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return media.InternalWrap(media.StageCommit, err, "write temporary file")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return media.InternalWrap(media.StageCommit, err, "sync temporary file")
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return media.InternalWrap(media.StageCommit, err, "close temporary file")
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		cleanup()
		return media.InternalWrap(media.StageCommit, err, "chmod temporary file")
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return media.InternalWrap(media.StageCommit, err, "atomic rename %s", path)
	}
	if err := SyncDir(dir); err != nil {
		return err
	}
	return nil
}

// CommitOpenFile syncs/closes a temp file and atomically renames it to final.
func CommitOpenFile(tmp *os.File, final string) error {
	tmpName := tmp.Name()
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return media.InternalWrap(media.StageCommit, err, "sync %s", tmpName)
	}
	if err := tmp.Close(); err != nil {
		return media.InternalWrap(media.StageCommit, err, "close %s", tmpName)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return media.InternalWrap(media.StageCommit, err, "rename %s -> %s", tmpName, final)
	}
	return SyncDir(filepath.Dir(final))
}

// NewTempInSameDir creates the temporary output beside its final path.
func NewTempInSameDir(final string) (*os.File, error) {
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, media.InternalWrap(media.StageCommit, err, "create output directory")
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(final)+"-*")
	if err != nil {
		return nil, media.InternalWrap(media.StageCommit, err, "create same-directory temp")
	}
	return f, nil
}

func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return media.InternalWrap(media.StageCommit, err, "open directory for sync: %s", dir)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return media.InternalWrap(media.StageCommit, err, "sync directory %s", dir)
	}
	return nil
}

// SHA256File returns hex digest and size.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, media.InternalWrap(media.StageInput, err, "open for digest: %s", path)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, media.InternalWrap(media.StageInput, err, "digest %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func DigestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
