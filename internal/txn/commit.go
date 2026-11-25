package txn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"relaycheck/internal/media"
)

// SegmentFileName is the canonical fragment file name for an index.
func SegmentFileName(index int) string {
	return filepath.Join("segments", fmt.Sprintf("seg-%06d.rlc", index))
}

// ManifestEntry locates one committed fragment and its content hash.
type ManifestEntry struct {
	Index      int    `json:"index"`
	File       string `json:"file"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	KeyframeID int    `json:"keyframe_id"`
}

// Manifest is the atomically committed segment list.
type Manifest struct {
	Version        int             `json:"version"`
	Seed           int64           `json:"seed"`
	TargetRate     int             `json:"target_rate"`
	Layout         string          `json:"layout"`
	IntegratedLUFS float64         `json:"integrated_lufs"`
	SegmentCount   int             `json:"segment_count"`
	Entries        []ManifestEntry `json:"entries"`
}

// Committer owns the WAL and the segment directory.
type Committer struct {
	Dir    string
	SegDir string
	WAL    *WAL
}

// NewCommitter opens/creates the layout under dir.
func NewCommitter(dir string) (*Committer, *media.Fault) {
	segDir := filepath.Join(dir, "segments")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, media.NewFault(media.CodeCommitFailed, "create segments dir: %v", err)
	}
	w, f := OpenWAL(dir)
	if f != nil {
		return nil, f
	}
	return &Committer{Dir: dir, SegDir: segDir, WAL: w}, nil
}

// Close syncs the WAL.
func (c *Committer) Close() { _ = c.WAL.Close() }

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (c *Committer) finalPath(index int) string {
	return filepath.Join(c.Dir, SegmentFileName(index))
}
func (c *Committer) tmpPath(index int) string { return c.finalPath(index) + ".tmp" }

// PrepareSegment checksumms, durably stages bytes and records prepared intent.
func (c *Committer) PrepareSegment(index int, b []byte) (string, *media.Fault) {
	sha := shaHex(b)
	if _, f := c.WAL.Append(Record{Phase: PhaseBegin, Segment: index}); f != nil {
		return "", f
	}
	tmp := c.tmpPath(index)
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", media.NewFault(media.CodeCommitFailed, "stage segment %d: %v", index, err)
	}
	if _, err := file.Write(b); err != nil {
		_ = file.Close()
		return "", media.NewFault(media.CodeCommitFailed, "write stage %d: %v", index, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", media.NewFault(media.CodeCommitFailed, "sync stage %d: %v", index, err)
	}
	if err := file.Close(); err != nil {
		return "", media.NewFault(media.CodeCommitFailed, "close stage %d: %v", index, err)
	}
	if _, f := c.WAL.Append(Record{Phase: PhasePrepared, Segment: index, SHA256: sha, Size: int64(len(b))}); f != nil {
		return "", f
	}
	return sha, nil
}

// CommitPrepared atomically renames the staged fragment and records commit.
func (c *Committer) CommitPrepared(index int, sha string) *media.Fault {
	if err := os.Rename(c.tmpPath(index), c.finalPath(index)); err != nil {
		return media.NewFault(media.CodeCommitFailed, "atomic commit segment %d: %v", index, err)
	}
	if f := SyncDir(c.SegDir); f != nil {
		return f
	}
	if _, f := c.WAL.Append(Record{Phase: PhaseCommitted, Segment: index, SHA256: sha}); f != nil {
		return f
	}
	return nil
}

// CommitSegment prepares then commits one fragment.
func (c *Committer) CommitSegment(index int, b []byte) (string, *media.Fault) {
	sha, f := c.PrepareSegment(index, b)
	if f != nil {
		return "", f
	}
	if f := c.CommitPrepared(index, sha); f != nil {
		return "", f
	}
	return sha, nil
}

// PendingTxn is a prepared segment lacking a committed record.
type PendingTxn struct {
	Segment int
	SHA     string
}

// RecoverPending returns prepared-but-not-committed transactions in order.
func (c *Committer) RecoverPending() ([]PendingTxn, *media.Fault) {
	recs, f := c.WAL.ReadAll()
	if f != nil {
		return nil, f
	}
	committed := map[int]bool{}
	preparedSHA := map[int]string{}
	for _, r := range recs {
		switch r.Phase {
		case PhasePrepared:
			preparedSHA[r.Segment] = r.SHA256
		case PhaseCommitted:
			committed[r.Segment] = true
		}
	}
	var out []PendingTxn
	for idx, sha := range preparedSHA {
		if !committed[idx] {
			out = append(out, PendingTxn{Segment: idx, SHA: sha})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Segment < out[j].Segment })
	return out, nil
}

// ResolvePending verifies deterministic bytes against the recorded prepared
// hash and replays the atomic commit. Hash mismatch is unrecoverable integrity.
func (c *Committer) ResolvePending(p PendingTxn, b []byte) *media.Fault {
	if got := shaHex(b); got != p.SHA {
		return media.NewFault(media.CodeChecksumMismatch, "replay segment %d hash %s != wal %s", p.Segment, got, p.SHA).
			WithTrack(1)
	}
	tmp := c.tmpPath(p.Segment)
	if file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644); err == nil {
		_, _ = file.Write(b)
		_ = file.Sync()
		_ = file.Close()
	}
	return c.CommitPrepared(p.Segment, p.SHA)
}

// WriteManifest atomically writes and fsyncs manifest.json.
func (c *Committer) WriteManifest(m *Manifest) *media.Fault {
	m.Version = 1
	if m.SegmentCount == 0 {
		m.SegmentCount = len(m.Entries)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return media.NewFault(media.CodeCommitFailed, "marshal manifest: %v", err)
	}
	final := filepath.Join(c.Dir, "manifest.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return media.NewFault(media.CodeCommitFailed, "write manifest tmp: %v", err)
	}
	if f, err := os.Open(tmp); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	if err := os.Rename(tmp, final); err != nil {
		return media.NewFault(media.CodeCommitFailed, "atomic manifest: %v", err)
	}
	if f := SyncDir(c.Dir); f != nil {
		return f
	}
	_, f := c.WAL.Append(Record{Phase: PhaseManifest, Segment: m.SegmentCount})
	return f
}
