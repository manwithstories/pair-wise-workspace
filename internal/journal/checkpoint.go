// Package journal persists recovery checkpoints so an interrupted run can
// resume at the last fully committed packet boundary instead of restarting.
//
// A checkpoint is keyed by (asset id, track id) and records the inclusive
// range of input packets already committed, plus three guards that must match
// for the checkpoint to be reusable:
//
//   - the source digest (the primary container hash),
//   - the adapter name, and
//   - the adapter version.
//
// If any guard differs the old checkpoint is quarantined (moved aside, never
// deleted) and the asset is reprocessed from the start. This is what prevents
// a stale checkpoint from silently skipping packets after a re-master.
//
// All writes go through a temp file and an atomic rename, so a crash mid-write
// leaves the previous checkpoint intact rather than a half-written one.
package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Version is the on-disk checkpoint schema version.
const Version = 1

// State is the per-(asset,track) progress record.
type State struct {
	// AssetID is the manifest asset identity.
	AssetID string `json:"asset_id"`
	// TrackID is the source track id (MP4 track_ID or RTP SSRC).
	TrackID uint32 `json:"track_id"`
	// CommittedThrough is the highest input packet index committed to output.
	// A value of -1 means nothing committed yet.
	CommittedThrough int64 `json:"committed_through"`
	// CommitCount is how many packets were committed (CommittedThrough+1 for a
	// contiguous run; kept explicit so a resumed run can cross-check).
	CommitCount int64 `json:"commit_count"`
	// SourceSHA256 guards against a changed source.
	SourceSHA256 string `json:"source_sha256"`
	// Adapter guards against a changed parser.
	Adapter string `json:"adapter"`
	// AdapterVersion guards against a changed parser version.
	AdapterVersion int `json:"adapter_version"`
	// ConfigDigest summarises the effective parse options.
	ConfigDigest string `json:"config_digest"`
	// OutputSHA256 is the hash of the committed output prefix, used to detect
	// output tampering across a resume.
	OutputSHA256 string `json:"output_sha256"`
	// UpdatedAt is informational; excluded from determinism checks.
	UpdatedAt string `json:"updated_at"`
}

// Journal is a directory of checkpoints.
type Journal struct {
	dir string
}

// Open creates (or opens) a checkpoint directory.
func Open(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Journal{dir: dir}, nil
}

// Dir returns the checkpoint directory.
func (j *Journal) Dir() string { return j.dir }

func fileName(assetID string, trackID uint32) string {
	return fmt.Sprintf("%s__track-%d.json", sanitize(assetID), trackID)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "asset"
	}
	return b.String()
}

// Guards are the reusable-or-quarantine inputs.
type Guards struct {
	SourceSHA256   string
	Adapter        string
	AdapterVersion int
	ConfigDigest   string
}

// Load returns the stored state for (asset,track) if it is still valid against
// g. When the guards mismatch the state is quarantined and load reports
// (nil, nil) with quarantined=true, signalling "start over".
func (j *Journal) Load(assetID string, trackID uint32, g Guards) (st *State, quarantined bool, err error) {
	path := filepath.Join(j.dir, fileName(assetID, trackID))
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		// A corrupt checkpoint is itself unrecoverable state: quarantine it.
		if qerr := j.quarantine(path); qerr != nil {
			return nil, false, qerr
		}
		return nil, true, fmt.Errorf("journal: corrupt checkpoint for %s track %d: %w", assetID, trackID, err)
	}
	if s.SourceSHA256 != g.SourceSHA256 || s.Adapter != g.Adapter ||
		s.AdapterVersion != g.AdapterVersion || s.ConfigDigest != g.ConfigDigest {
		if qerr := j.quarantine(path); qerr != nil {
			return nil, false, qerr
		}
		return nil, true, nil
	}
	return &s, false, nil
}

// Save atomically writes st. Existing checkpoints are replaced only after the
// new file is fully on disk.
func (j *Journal) Save(st *State) error {
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(j.dir, fileName(st.AssetID, st.TrackID))
	tmp, err := os.CreateTemp(j.dir, ".ckpt-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// quarantine moves a stale/corrupt checkpoint aside. It never deletes: an
// operator may need the old range to reconcile the audit trail.
func (j *Journal) quarantine(path string) error {
	qdir := filepath.Join(j.dir, "quarantine")
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		return err
	}
	base := filepath.Base(path)
	dest := filepath.Join(qdir, fmt.Sprintf("%s.%d", base, time.Now().UnixNano()))
	return os.Rename(path, dest)
}

// Quarantined lists quarantined checkpoint file names, sorted.
func (j *Journal) Quarantined() ([]string, error) {
	qdir := filepath.Join(j.dir, "quarantine")
	ents, err := os.ReadDir(qdir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ConfigDigest hashes the effective parse options so a change in limits or
// payload handling invalidates checkpoints.
func ConfigDigest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TrackProgress is a resume plan for one track.
type TrackProgress struct {
	TrackID uint32
	// ResumeAfter is the packet index to resume from (CommittedThrough+1), or
	// 0 to start fresh.
	ResumeAfter int64
	// Committed packets already written in a previous run.
	Committed int64
	// Quarantined is true when a prior checkpoint was set aside.
	Quarantined bool
}

// Plan builds per-track resume progress for an asset. For every track it loads
// or quarantines the checkpoint, so the caller can decide whether to resume or
// reprocess.
func (j *Journal) Plan(assetID string, trackIDs []uint32, g Guards) ([]TrackProgress, error) {
	out := make([]TrackProgress, 0, len(trackIDs))
	for _, id := range trackIDs {
		st, q, err := j.Load(assetID, id, g)
		if err != nil && !q {
			return nil, err
		}
		tp := TrackProgress{TrackID: id, Quarantined: q}
		if st != nil {
			tp.ResumeAfter = st.CommittedThrough + 1
			tp.Committed = st.CommitCount
		}
		out = append(out, tp)
	}
	return out, nil
}

// ParseTrackID parses a checkpoint file name back into its track id. Used by
// tests and audit tooling.
func ParseTrackID(name string) (uint32, error) {
	i := strings.LastIndex(name, "__track-")
	if i < 0 {
		return 0, fmt.Errorf("journal: not a checkpoint name: %s", name)
	}
	rest := strings.TrimPrefix(name[i:], "__track-")
	rest = strings.TrimSuffix(rest, ".json")
	v, err := strconv.ParseUint(rest, 10, 32)
	return uint32(v), err
}
