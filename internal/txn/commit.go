package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"bcast/internal/media"
)

const CommitMarker = "COMMIT.json"

// CommitRecord is the atomically published batch marker.
type CommitRecord struct {
	Generation string            `json:"generation"`
	SHA256     string            `json:"sha256"`
	Files      map[string]string `json:"files"`
}

// Txn binds one staging generation to the write-ahead log.
type Txn struct {
	OutDir   string
	Gen      string
	StageDir string
	FinalDir string
	WAL      *WAL
}

// Begin creates a staging generation and writes the begin WAL record.
func Begin(out, gen string) (*Txn, error) {
	wal, err := OpenWAL(out)
	if err != nil {
		return nil, err
	}
	t := &Txn{
		OutDir:   out,
		Gen:      gen,
		StageDir: filepath.Join(out, "staging", gen),
		FinalDir: filepath.Join(out, "committed", gen),
		WAL:      wal,
	}
	if err := os.MkdirAll(t.StageDir, 0o755); err != nil {
		_ = wal.Close()
		return nil, media.IO("txn", t.StageDir, 0, 0, err)
	}
	if err := t.Log("txn", "begin", "generation="+gen); err != nil {
		_ = wal.Close()
		return nil, err
	}
	return t, nil
}

// Log appends a stage record.
func (t *Txn) Log(stage, status, summary string) error {
	return t.WAL.Append(WALEntry{Stage: stage, Generation: t.Gen, Status: status, Summary: summary})
}

// WriteArtifact atomically writes a named artifact into staging.
func (t *Txn) WriteArtifact(name string, data []byte) error {
	if filepath.IsAbs(name) || name == "SHA256SUMS" || name == CommitMarker {
		return media.Corrupt("txn", "invalid_artifact_name", name, 0, -1, "reserved or absolute artifact name")
	}
	path := filepath.Join(t.StageDir, name)
	if err := WriteFileAtomic(path, data); err != nil {
		return err
	}
	return t.Log(name, "staged", "bytes="+itoa(len(data)))
}

// Commit hashes staged artifacts, atomically moves the generation and publishes
// COMMIT.json.
func (t *Txn) Commit() (*CommitRecord, error) {
	files, sums, aggregate, err := hashStage(t.StageDir)
	if err != nil {
		return nil, err
	}
	sumsPath := filepath.Join(t.StageDir, "SHA256SUMS")
	if err := WriteFileAtomic(sumsPath, []byte(sums)); err != nil {
		return nil, err
	}
	files["SHA256SUMS"] = aggregate
	rec := &CommitRecord{Generation: t.Gen, SHA256: aggregate, Files: files}
	marker, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, media.Corrupt("commit", "encode_marker", CommitMarker, 0, -1, "%v", err)
	}
	marker = append(marker, '\n')
	if err := t.Log("commit", "prepared", "sha256="+aggregate); err != nil {
		return nil, err
	}

	if _, err := os.Stat(t.FinalDir); err == nil {
		// A prior attempt renamed the generation but died before the marker.
		if err := os.RemoveAll(t.FinalDir); err != nil {
			return nil, media.IO("commit", t.FinalDir, 0, 0, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(t.FinalDir), 0o755); err != nil {
		return nil, media.IO("commit", filepath.Dir(t.FinalDir), 0, 0, err)
	}
	if err := os.Rename(t.StageDir, t.FinalDir); err != nil {
		return nil, media.IO("commit", t.FinalDir, 0, 0, err)
	}
	markerPath := filepath.Join(t.OutDir, CommitMarker)
	if err := WriteFileAtomic(markerPath, marker); err != nil {
		return nil, err
	}
	if err := t.Log("commit", "committed", "sha256="+aggregate); err != nil {
		return nil, err
	}
	if err := t.WAL.Close(); err != nil {
		return nil, err
	}
	t.WAL = nil
	return rec, nil
}

// Close releases the WAL if Commit did not already close it.
func (t *Txn) Close() error {
	if t == nil || t.WAL == nil {
		return nil
	}
	return t.WAL.Close()
}

// LoadCommit reads and validates the published commit marker.
func LoadCommit(out string) (*CommitRecord, error, bool) {
	path := filepath.Join(out, CommitMarker)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, false
		}
		return nil, media.IO("resume", path, 0, 0, err), false
	}
	var rec CommitRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, media.Corrupt("resume", "invalid_commit_marker", path, 0, -1, "%v", err), true
	}
	return &rec, nil, true
}

// StagingGenerations lists uncommitted staging directories.
func StagingGenerations(out string) ([]string, error) {
	root := filepath.Join(out, "staging")
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, media.IO("resume", root, 0, 0, err)
	}
	var gens []string
	for _, e := range ents {
		if e.IsDir() {
			gens = append(gens, e.Name())
		}
	}
	sort.Strings(gens)
	return gens, nil
}

// RollbackStaging removes every staging generation and returns the count.
func RollbackStaging(out string) (int, error) {
	gens, err := StagingGenerations(out)
	if err != nil {
		return 0, err
	}
	for _, gen := range gens {
		path := filepath.Join(out, "staging", gen)
		if err := os.RemoveAll(path); err != nil {
			return 0, media.IO("rollback", path, 0, 0, err)
		}
	}
	if len(gens) > 0 {
		if wal, err := OpenWAL(out); err == nil {
			_ = wal.Append(WALEntry{Stage: "rollback", Status: "rolled_back", Summary: "generations=" + itoa(len(gens))})
			_ = wal.Close()
		}
	}
	return len(gens), nil
}

func hashStage(dir string) (map[string]string, string, string, error) {
	relPaths := []string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return media.IO("commit", path, 0, 0, err)
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return media.Corrupt("commit", "relative_path", path, 0, -1, "%v", err)
		}
		if rel == "SHA256SUMS" {
			return nil
		}
		relPaths = append(relPaths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, "", "", err
	}
	sort.Strings(relPaths)
	files := map[string]string{}
	var sums string
	for _, rel := range relPaths {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, "", "", media.IO("commit", rel, 0, 0, err)
		}
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		files[rel] = h
		sums += h + "  " + rel + "\n"
	}
	agg := sha256.Sum256([]byte(sums))
	return files, sums, hex.EncodeToString(agg[:]), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
