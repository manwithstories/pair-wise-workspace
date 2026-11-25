package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediacheck/internal/media"
)

// Recovery contains only committed stages whose final artifact hash verifies.
type Recovery struct {
	Root      string
	TxID      string
	Records   []Record
	Completed map[string]Record
	Invalid   []string
}

// Recover reads the WAL and discards committed records whose atomic artifact
// is missing or corrupted. Prepared-but-uncommitted stages are never reused.
func Recover(root, txID string) (*Recovery, error) {
	records, err := ReadRecords(root)
	if err != nil {
		return nil, err
	}
	r := &Recovery{Root: root, TxID: txID, Records: records, Completed: map[string]Record{}}
	latest := map[string]Record{}
	for _, rec := range records {
		if rec.Status != "committed" {
			continue
		}
		if txID != "" && rec.TxID != txID {
			continue
		}
		latest[rec.Stage] = rec
	}
	for stage, rec := range latest {
		ok, err := verifyFinalArtifact(root, rec)
		if err != nil {
			return nil, err
		}
		if ok {
			r.Completed[stage] = rec
		} else {
			r.Invalid = append(r.Invalid, stage)
		}
	}
	sort.Strings(r.Invalid)
	return r, nil
}

func verifyFinalArtifact(root string, rec Record) (bool, error) {
	path, err := SafeJoin(root, rec.FinalRel)
	if err != nil {
		return false, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, media.E(media.StageRecover, media.KindResource, rec.Offset, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return false, media.E(media.StageRecover, media.KindResource, rec.Offset, err)
	}
	if rec.Size > 0 && n != rec.Size {
		return false, nil
	}
	if rec.SHA256 != "" && hex.EncodeToString(h.Sum(nil)) != rec.SHA256 {
		return false, nil
	}
	return true, nil
}

// Has reports whether a stage can safely be reused.
func (r *Recovery) Has(stage string) bool {
	if r == nil {
		return false
	}
	_, ok := r.Completed[stage]
	return ok
}

// Record returns the verified committed record for a stage.
func (r *Recovery) Record(stage string) (Record, bool) {
	if r == nil {
		return Record{}, false
	}
	rec, ok := r.Completed[stage]
	return rec, ok
}

// Stages returns recovered stage names in canonical pipeline order.
func (r *Recovery) Stages(order ...string) []string {
	if r == nil {
		return nil
	}
	var out []string
	if len(order) > 0 {
		for _, stage := range order {
			if r.Has(stage) {
				out = append(out, stage)
			}
		}
		return out
	}
	for stage := range r.Completed {
		out = append(out, stage)
	}
	sort.Strings(out)
	return out
}

// CleanTemporaryArtifacts removes *.tmp files left by interrupted stages.
func CleanTemporaryArtifacts(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".tmp") {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return media.E(media.StageRecover, media.KindResource, -1, err)
		}
		return nil
	})
}

// HashFile streams a file hash.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
