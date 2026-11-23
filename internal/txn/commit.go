// Package txn runs pipeline stages with atomic commit, rollback and
// resume. A stage is committed only after its outputs are durable and
// their digest is recorded in the WAL; on restart only uncommitted
// stages are redone and partial outputs are rolled back.
package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"medrelay/internal/journal"
	"medrelay/internal/timeline"
)

// ErrInterrupt simulates a process crash between a stage's work and its
// commit record.
var ErrInterrupt = errors.New("txn: interrupted before commit")

// Stage is one pipeline step. Run must write its outputs into dir.
type Stage struct {
	Name string
	Run  func(dir string) error
}

// Runner executes stages against a WAL and a stage-output root.
type Runner struct {
	WAL  *journal.WAL
	Root string

	// InterruptAfter, when set, returns ErrInterrupt after that stage's
	// outputs are written but before its commit record.
	InterruptAfter string

	// Replayed lists stages actually executed in this run.
	Replayed []string
	// Rollbacks counts rolled-back uncommitted stage outputs.
	Rollbacks int
}

// DigestDir hashes all files under dir (names + contents, sorted).
func DigestDir(dir string) (string, error) {
	h := sha256.New()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, rel := range files {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return "", err
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Execute runs stages in order, skipping stages with a valid commit.
func (r *Runner) Execute(stages []Stage) error {
	inputSHA := ""
	for _, st := range stages {
		dir := filepath.Join(r.Root, st.Name)
		if c, ok := r.WAL.Committed(st.Name); ok {
			digest, err := DigestDir(dir)
			if err == nil && digest == c.OutputSHA {
				inputSHA = c.OutputSHA
				continue // committed and intact: skip
			}
			// Committed but outputs damaged: roll back and redo.
			if err := r.rollback(dir); err != nil {
				return err
			}
		} else if _, err := os.Stat(dir); err == nil {
			// Uncommitted partial outputs from an interrupted run.
			if err := r.rollback(dir); err != nil {
				return err
			}
		}
		if err := r.WAL.Append(journal.Record{Stage: st.Name, Status: journal.StatusBegin, InputSHA: inputSHA}); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := st.Run(dir); err != nil {
			return err
		}
		digest, err := DigestDir(dir)
		if err != nil {
			return err
		}
		if err := r.WAL.Append(journal.Record{Stage: st.Name, Status: journal.StatusDone, InputSHA: inputSHA, OutputSHA: digest}); err != nil {
			return err
		}
		r.Replayed = append(r.Replayed, st.Name)
		if r.InterruptAfter == st.Name {
			return ErrInterrupt
		}
		if err := r.WAL.Append(journal.Record{Stage: st.Name, Status: journal.StatusCommit, InputSHA: inputSHA, OutputSHA: digest}); err != nil {
			return err
		}
		inputSHA = digest
	}
	return nil
}

// Verify checks that every committed stage's on-disk outputs still match
// its committed digest.
func (r *Runner) Verify(stages []Stage) error {
	for _, st := range stages {
		c, ok := r.WAL.Committed(st.Name)
		if !ok {
			return timeline.New(timeline.ErrDigest, st.Name, 0, 0, "stage has no commit record")
		}
		digest, err := DigestDir(filepath.Join(r.Root, st.Name))
		if err != nil {
			return err
		}
		if digest != c.OutputSHA {
			return timeline.New(timeline.ErrDigest, st.Name, 0, 0,
				fmt.Sprintf("output digest %s != committed %s", digest, c.OutputSHA))
		}
	}
	return nil
}

func (r *Runner) rollback(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	r.Rollbacks++
	return nil
}
