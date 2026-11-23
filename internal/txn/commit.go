package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/example/mediad/internal/model"
)

// CrashPoint identifies where a simulated process interruption fires.
type CrashPoint string

const (
	CrashNone         CrashPoint = ""
	CrashAfterWrite   CrashPoint = "write"
	CrashAfterVerify  CrashPoint = "verify"
	CrashBeforeCommit CrashPoint = "commit"
)

// CommitResult reports what one segment run did.
type CommitResult struct {
	SegmentID  string
	OutputPath string
	Hash       string
	RolledBack bool
	Reused     bool // idempotent replay reused a committed artifact
}

// Committer coordinates the temp area, journal and atomic rename for
// one working directory.
type Committer struct {
	root     string // workspace root
	tmpDir   string // uncommitted artifacts live here
	finalDir string // committed artifacts live here
	journal  *Journal
	crash    CrashPoint
	// RollbackCount increments whenever an uncommitted temp artifact
	// is discarded during recovery or verification failure.
	RollbackCount int
}

// NewCommitter prepares the directory layout.
func NewCommitter(root string, journal *Journal, crash CrashPoint) *Committer {
	return &Committer{
		root:     root,
		tmpDir:   filepath.Join(root, "tmp"),
		finalDir: filepath.Join(root, "segments"),
		journal:  journal,
		crash:    crash,
	}
}

// Prepare ensures temp and final directories exist.
func (c *Committer) Prepare() error {
	for _, d := range []string{c.tmpDir, c.finalDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return model.Wrap(model.KindState, "txn", "prepare", d, err)
		}
	}
	return nil
}

// SetJournal swaps the journal handle after crash recovery reopens it.
func (c *Committer) SetJournal(j *Journal) { c.journal = j }

// ClearCrash disables the injected interruption after the first crash
// has been recovered so subsequent segments run to completion.
func (c *Committer) ClearCrash() { c.crash = CrashNone }

// Run executes the write -> verify -> commit flow for one segment.
// payload produces the artifact bytes; verifier may reject them. A
// crash point aborts the process model by returning ErrInjectedCrash.
func (c *Committer) Run(segmentID string, payload []byte, verifier func(hash string) error) (*CommitResult, error) {
	finalPath := filepath.Join(c.finalDir, segmentID+".part")
	tmpPath := filepath.Join(c.tmpDir, segmentID+".part.tmp")

	// Idempotent fast path: a prior run already committed this segment.
	if info, err := os.Stat(finalPath); err == nil && !info.IsDir() {
		h, err := hashFile(finalPath)
		if err == nil {
			return &CommitResult{SegmentID: segmentID, OutputPath: finalPath,
				Hash: h, Reused: true}, nil
		}
	}

	if _, err := c.journal.Begin(StageWrite, segmentID, shortHash(payload)); err != nil {
		return nil, err
	}
	if err := os.WriteFile(tmpPath, payload, 0o644); err != nil {
		return nil, model.Wrap(model.KindTransient, "txn", "write",
			tmpPath, err)
	}
	if c.crash == CrashAfterWrite {
		return nil, ErrInjectedCrash
	}
	h, err := hashFile(tmpPath)
	if err != nil {
		return nil, err
	}
	if _, err := c.journal.Done(StageWrite, segmentID, tmpPath, h); err != nil {
		return nil, err
	}

	// Verification boundary.
	if _, err := c.journal.Begin(StageVerify, segmentID, h); err != nil {
		return nil, err
	}
	if verifier != nil {
		if err := verifier(h); err != nil {
			c.rollback(tmpPath)
			return nil, err
		}
	}
	if c.crash == CrashAfterVerify {
		return nil, ErrInjectedCrash
	}
	if _, err := c.journal.Done(StageVerify, segmentID, tmpPath, h); err != nil {
		return nil, err
	}

	// Atomic commit: rename temp to final, journaled on both sides.
	if _, err := c.journal.Begin(StageCommit, segmentID, h); err != nil {
		return nil, err
	}
	if c.crash == CrashBeforeCommit {
		return nil, ErrInjectedCrash
	}
	if err := atomicRename(tmpPath, finalPath); err != nil {
		c.rollback(tmpPath)
		return nil, err
	}
	if _, err := c.journal.Done(StageCommit, segmentID, finalPath, h); err != nil {
		return nil, err
	}
	return &CommitResult{SegmentID: segmentID, OutputPath: finalPath, Hash: h}, nil
}

// Recover is invoked at startup. It rolls back every temp artifact whose
// commit stage never reached a complete journal boundary, and returns
// the segment IDs that must be rerun.
func (c *Committer) Recover(replay Replay) ([]string, error) {
	if err := c.Prepare(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(c.tmpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, model.Wrap(model.KindState, "txn", "recover", c.tmpDir, err)
	}
	var rerun []string
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		segmentID := trimSuffix(name, ".part.tmp")
		// Only fully journaled commits may survive; everything else in
		// the temp area is uncommitted and rolls back.
		if replay.IsDone(StageCommit, segmentID) {
			continue
		}
		c.rollback(filepath.Join(c.tmpDir, name))
		rerun = append(rerun, segmentID)
	}
	return rerun, nil
}

func (c *Committer) rollback(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return
	}
	c.RollbackCount++
}

// ErrInjectedCrash signals a deliberately simulated interruption.
var ErrInjectedCrash = model.New(model.KindTransient, "txn", "crash",
	"simulated process interruption")

func atomicRename(old, new string) error {
	// os.Rename is atomic within one filesystem on macOS.
	if err := os.Rename(old, new); err != nil {
		return model.Wrap(model.KindState, "txn", "commit", new, err)
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", model.Wrap(model.KindChecksum, "txn", "hash", path, err)
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", model.Wrap(model.KindChecksum, "txn", "hash", path, err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func trimSuffix(s, suffix string) string {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
}
