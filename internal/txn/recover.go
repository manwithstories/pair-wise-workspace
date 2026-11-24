package txn

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"relay/internal/media"
)

// RecoverySummary reports what Resume found and did.
type RecoverySummary struct {
	Replayed    int
	Committed   bool
	RolledBack  bool
	CommitID    [32]byte
	Manifest    *Manifest
	ManifestSum [32]byte
}

// Resume replays the WAL and finishes an interrupted transaction. It is
// idempotent: a second invocation finds CURRENT already published and never
// commits the same samples twice.
func Resume(workDir string) (*RecoverySummary, error) {
	wal, err := OpenWAL(workDir)
	if err != nil {
		return nil, err
	}
	defer wal.Close()
	recs, err := wal.ReadAll()
	if err != nil {
		return nil, media.NewError(media.ErrWAL, 0, err.Error())
	}
	if len(recs) == 0 {
		return &RecoverySummary{}, nil
	}
	// Only the latest (open or last) transaction is recovered.
	st := &replayState{}
	for _, rec := range recs {
		if err := st.apply(rec); err != nil {
			return nil, err
		}
	}
	sum := &RecoverySummary{Replayed: len(st.staged), CommitID: st.commit}

	// Clean transaction: nothing left to do.
	if st.ended {
		if st.manifest != nil {
			sum.Manifest = st.manifest
			sum.ManifestSum = ManifestHash(*st.manifest)
			sum.Committed = st.committed
		}
		return sum, nil
	}
	// Explicit rollback was logged before the crash: finish cleanup only.
	if st.rollback {
		if err := os.RemoveAll(filepath.Join(workDir, "staging")); err != nil {
			return nil, err
		}
		if _, err := wal.Append(RecEnd, st.commit[:]); err != nil {
			return nil, err
		}
		sum.RolledBack = true
		return sum, nil
	}
	current, haveCurrent := readCurrent(workDir)
	if st.committed && current == st.commit {
		// Crash between the CURRENT switch and the End record: close it out.
		if err := os.RemoveAll(filepath.Join(workDir, "staging")); err != nil {
			return nil, err
		}
		if _, err := wal.Append(RecEnd, st.commit[:]); err != nil {
			return nil, err
		}
		sum.Committed = true
		if st.manifest != nil {
			sum.Manifest = st.manifest
			sum.ManifestSum = ManifestHash(*st.manifest)
		}
		return sum, nil
	}
	// Open staged transaction: verify staged bytes before publishing.
	if err := st.verify(workDir); err != nil {
		// Corrupt staging: roll back without touching CURRENT.
		if _, lerr := wal.Append(RecRollback, st.commit[:]); lerr != nil {
			return nil, errors.Join(err, lerr)
		}
		if rerr := os.RemoveAll(filepath.Join(workDir, "staging")); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		if _, eerr := wal.Append(RecEnd, st.commit[:]); eerr != nil {
			return nil, errors.Join(err, eerr)
		}
		sum.RolledBack = true
		return sum, err
	}
	m := st.buildManifest()
	body := encodeManifest(m)
	committedDir := filepath.Join(workDir, "committed", fmt.Sprintf("%x", st.commit[:8]))
	if err := os.MkdirAll(committedDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(committedDir, "manifest.bin"), body, 0o600); err != nil {
		return nil, err
	}
	if !haveCurrent || current != st.commit {
		tmp := filepath.Join(workDir, "CURRENT.tmp")
		if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%x\n", st.commit)), 0o600); err != nil {
			return nil, err
		}
		if _, err := wal.Append(RecCommit, st.commit[:]); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, filepath.Join(workDir, "CURRENT")); err != nil {
			return nil, err
		}
	}
	if err := os.RemoveAll(filepath.Join(workDir, "staging")); err != nil {
		return nil, err
	}
	if _, err := wal.Append(RecEnd, st.commit[:]); err != nil {
		return nil, err
	}
	sum.Committed = true
	sum.Manifest = &m
	sum.ManifestSum = ManifestHash(m)
	return sum, nil
}

// readCurrent returns the commit id stored in CURRENT, if the pointer exists.
// The file holds 64 lowercase hex digits (an optional trailing newline is
// tolerated), matching the deterministic commit ids written at commit time.
func readCurrent(workDir string) ([32]byte, bool) {
	var id [32]byte
	data, err := os.ReadFile(filepath.Join(workDir, "CURRENT"))
	if err != nil || len(data) < 64 {
		return id, false
	}
	for i := 0; i < 32; i++ {
		hi, ok1 := hexNibble(data[i*2])
		lo, ok2 := hexNibble(data[i*2+1])
		if !ok1 || !ok2 {
			return id, false
		}
		id[i] = hi<<4 | lo
	}
	return id, true
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}

// CurrentCommitID exposes the CURRENT pointer for callers that need to prove
// it remained unchanged across a rollback.
func CurrentCommitID(workDir string) ([32]byte, bool) { return readCurrent(workDir) }

type replayState struct {
	commit    [32]byte
	staged    []StageRecord
	manifest  *Manifest
	committed bool
	rollback  bool
	ended     bool
}

func (s *replayState) apply(rec Record) error {
	switch rec.Type {
	case RecBegin:
		if len(rec.Payload) != 32 {
			return errors.New("wal: bad begin payload")
		}
		copy(s.commit[:], rec.Payload)
		s.staged = nil
		s.manifest = nil
		s.committed, s.rollback, s.ended = false, false, false
	case RecStage:
		dec, err := DecodeStage(rec.Payload)
		if err != nil {
			return err
		}
		s.staged = append(s.staged, dec)
	case RecManifest:
		m, err := DecodeManifest(rec.Payload)
		if err != nil {
			return err
		}
		s.manifest = &m
	case RecCommit:
		s.committed = true
	case RecRollback:
		s.rollback = true
	case RecEnd:
		s.ended = true
	default:
		return errors.New("wal: unknown record type")
	}
	return nil
}

func (s *replayState) verify(workDir string) error {
	for _, rec := range s.staged {
		abs := filepath.Join(workDir, rec.RelPath)
		if err := checkWithin(workDir, abs); err != nil {
			return err
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return media.NewError(media.ErrChecksumMismatch, rec.TrackID, "replay: staged sample missing")
		}
		if uint32(len(data)) != rec.Size {
			return media.NewError(media.ErrLengthMismatch, rec.TrackID, "replay: staged size mismatch")
		}
		if sha256.Sum256(data) != rec.SHA256 {
			return media.NewError(media.ErrChecksumMismatch, rec.TrackID, "replay: staged checksum mismatch")
		}
	}
	return nil
}

func (s *replayState) buildManifest() Manifest {
	if s.manifest != nil {
		return *s.manifest
	}
	m := Manifest{CommitID: s.commit}
	for _, rec := range s.staged {
		m.Entries = append(m.Entries, ManifestEntry{
			TrackID: rec.TrackID, Sequence: rec.Sequence,
			Size: rec.Size, SHA256: rec.SHA256,
		})
	}
	return m
}
