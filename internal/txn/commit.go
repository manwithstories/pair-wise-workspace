package txn

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"relay/internal/media"
)

// CrashPoint names the fault-injection moments recognised by a Txn.
const (
	CrashAfterStaged = "staged" // after samples+manifest staged, before commit
)

// ManifestEntry describes one committed sample for the deterministic manifest.
type ManifestEntry struct {
	TrackID  uint32
	Sequence uint64
	Size     uint32
	SHA256   [32]byte
}

// Manifest is the canonical, order-sensitive list of every output sample.
// Its serialised form is hashed to prove that repeated runs are identical.
type Manifest struct {
	CommitID [32]byte
	Entries  []ManifestEntry
}

// Txn is one staged, not-yet-committed output transaction.
type Txn struct {
	workDir string
	wal     *WAL
	commit  [32]byte
	staged  []StageRecord
	crashAt string
	closed  bool
}

// Begin opens a fresh transaction: the work dir is created and a Begin record
// is appended to the WAL. The commit id derives deterministically from the
// canonical sample identities supplied by the caller.
func Begin(workDir string, commitID [32]byte, crashPoint string) (*Txn, error) {
	if err := checkWithin(workDir, workDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(workDir, "staging"), 0o755); err != nil {
		return nil, err
	}
	wal, err := OpenWAL(workDir)
	if err != nil {
		return nil, err
	}
	t := &Txn{workDir: workDir, wal: wal, commit: commitID, crashAt: crashPoint}
	if _, err := wal.Append(RecBegin, commitID[:]); err != nil {
		wal.Close()
		return nil, err
	}
	return t, nil
}

// Stage writes one sample into the staging dir, hashes the bytes actually
// written, records offset/size/checksum in the WAL and refuses any sample
// whose bytes do not match the metadata hash or whose path escapes the work
// directory.
func (t *Txn) Stage(trackID uint32, seq uint64, fr *media.RawFrame) error {
	rel := filepath.Join("staging", fmt.Sprintf("track-%d", trackID),
		fmt.Sprintf("sample-%010d.bin", seq))
	abs := filepath.Join(t.workDir, rel)
	if err := checkWithin(t.workDir, abs); err != nil {
		return err
	}
	if len(fr.Payload) > media.MaxFrameBytes {
		return media.NewError(media.ErrLimitExceeded, fr.Meta.SourceID, "sample over limit")
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(abs, fr.Payload, 0o600); err != nil {
		return err
	}
	sum := sha256.Sum256(fr.Payload)
	if sum != fr.Meta.SHA256 {
		return media.NewError(media.ErrChecksumMismatch, fr.Meta.SourceID,
			"staged sample disagrees with metadata hash")
	}
	off := uint64(0)
	if len(t.staged) > 0 {
		prev := t.staged[len(t.staged)-1]
		off = prev.Offset + uint64(prev.Size)
	}
	rec := StageRecord{
		TrackID: trackID, Sequence: seq, Offset: off,
		Size: uint32(len(fr.Payload)), SHA256: sum, RelPath: rel,
	}
	if _, err := t.wal.Append(RecStage, EncodeStage(rec)); err != nil {
		return err
	}
	t.staged = append(t.staged, rec)
	return nil
}

// Finalize validates every staged file against its WAL record, writes the
// manifest and either injects the staged crash point or commits atomically.
func (t *Txn) Finalize() error {
	if err := t.verifyStaged(); err != nil {
		if rbErr := t.Rollback(); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	manifest := t.buildManifest()
	body := encodeManifest(manifest)
	rel := filepath.Join("staging", "manifest.bin")
	abs := filepath.Join(t.workDir, rel)
	if err := os.WriteFile(abs, body, 0o600); err != nil {
		return err
	}
	if _, err := t.wal.Append(RecManifest, body); err != nil {
		return err
	}
	if t.crashAt == CrashAfterStaged {
		// Simulated crash: log and staging are durable, CURRENT untouched.
		t.wal.Close()
		os.Exit(75)
	}
	return t.commitSwitch(manifest, body)
}

// Rollback discards staged content and records the rollback, leaving the
// CURRENT pointer exactly as it was before the transaction.
func (t *Txn) Rollback() error {
	if t.closed {
		return nil
	}
	if _, err := t.wal.Append(RecRollback, t.commit[:]); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(t.workDir, "staging")); err != nil {
		return err
	}
	if _, err := t.wal.Append(RecEnd, t.commit[:]); err != nil {
		return err
	}
	t.closed = true
	return t.wal.Close()
}

func (t *Txn) verifyStaged() error {
	for _, rec := range t.staged {
		abs := filepath.Join(t.workDir, rec.RelPath)
		if err := checkWithin(t.workDir, abs); err != nil {
			return err
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return media.NewError(media.ErrTruncated, rec.TrackID, "staged sample unreadable: "+err.Error())
		}
		if uint32(len(data)) != rec.Size {
			return media.NewError(media.ErrLengthMismatch, rec.TrackID, "staged size mismatch")
		}
		if sha256.Sum256(data) != rec.SHA256 {
			return media.NewError(media.ErrChecksumMismatch, rec.TrackID, "staged checksum mismatch")
		}
	}
	return nil
}

func (t *Txn) buildManifest() Manifest {
	m := Manifest{CommitID: t.commit}
	for _, rec := range t.staged {
		m.Entries = append(m.Entries, ManifestEntry{
			TrackID: rec.TrackID, Sequence: rec.Sequence,
			Size: rec.Size, SHA256: rec.SHA256,
		})
	}
	return m
}

func (t *Txn) commitSwitch(m Manifest, manifestBody []byte) error {
	committedDir := filepath.Join(t.workDir, "committed", fmt.Sprintf("%x", t.commit[:8]))
	if err := os.MkdirAll(committedDir, 0o755); err != nil {
		return err
	}
	// Publish manifest then swap CURRENT; both moves inside one work dir.
	finalManifest := filepath.Join(committedDir, "manifest.bin")
	if err := os.WriteFile(finalManifest, manifestBody, 0o600); err != nil {
		return err
	}
	cur := filepath.Join(t.workDir, "CURRENT")
	tmp := filepath.Join(t.workDir, "CURRENT.tmp")
	content := []byte(fmt.Sprintf("%x\n", t.commit))
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	if _, err := t.wal.Append(RecCommit, t.commit[:]); err != nil {
		return err
	}
	if err := os.Rename(tmp, cur); err != nil { // atomic on the same volume
		return err
	}
	if err := os.RemoveAll(filepath.Join(t.workDir, "staging")); err != nil {
		return err
	}
	if _, err := t.wal.Append(RecEnd, t.commit[:]); err != nil {
		return err
	}
	t.closed = true
	return t.wal.Close()
}

// ManifestHash returns the deterministic SHA-256 of the serialised manifest.
func ManifestHash(m Manifest) [32]byte {
	return sha256.Sum256(encodeManifest(m))
}

func encodeManifest(m Manifest) []byte {
	size := 32 + 4 + uint32(len(m.Entries))*(4+8+4+32)
	b := make([]byte, 0, size)
	b = append(b, m.CommitID[:]...)
	var cnt [4]byte
	binary.BigEndian.PutUint32(cnt[:], uint32(len(m.Entries)))
	b = append(b, cnt[:]...)
	for _, e := range m.Entries {
		var buf [16]byte
		binary.BigEndian.PutUint32(buf[0:4], e.TrackID)
		binary.BigEndian.PutUint64(buf[4:12], e.Sequence)
		binary.BigEndian.PutUint32(buf[12:16], e.Size)
		b = append(b, buf[:]...)
		b = append(b, e.SHA256[:]...)
	}
	return b
}

// DecodeManifest parses encodeManifest output.
func DecodeManifest(b []byte) (Manifest, error) {
	if len(b) < 36 {
		return Manifest{}, errors.New("txn: short manifest")
	}
	m := Manifest{}
	copy(m.CommitID[:], b[0:32])
	n := binary.BigEndian.Uint32(b[32:36])
	pos := 36
	for i := uint32(0); i < n; i++ {
		if pos+48 > len(b) {
			return Manifest{}, errors.New("txn: truncated manifest entry")
		}
		e := ManifestEntry{
			TrackID:  binary.BigEndian.Uint32(b[pos : pos+4]),
			Sequence: binary.BigEndian.Uint64(b[pos+4 : pos+12]),
			Size:     binary.BigEndian.Uint32(b[pos+12 : pos+16]),
		}
		copy(e.SHA256[:], b[pos+16:pos+48])
		m.Entries = append(m.Entries, e)
		pos += 48
	}
	return m, nil
}

// checkWithin refuses paths that escape the work directory, enforcing the
// local-only, workspace-confined input constraint.
func checkWithin(workDir, target string) error {
	absWork, err := filepath.Abs(filepath.Clean(workDir))
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	if absTarget != absWork && !strings.HasPrefix(absTarget, absWork+string(os.PathSeparator)) {
		return fmt.Errorf("txn: path %s escapes work directory %s", absTarget, absWork)
	}
	return nil
}
