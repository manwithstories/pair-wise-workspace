// Package checkpoint persists restoration progress so an interrupted run can
// resume without re-emitting sequence numbers or changing already-committed
// segment hashes. A checkpoint advances only after a segment's payload is on
// disk, fsynced and hash-verified. The on-disk document carries an HMAC-style
// checksum keyed on a run secret, so a tampered checkpoint is detected and
// reported as an integrity failure (exit code 3) rather than silently trusted.
package checkpoint

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/archive/tsrestore/internal/timeline"
)

// RenditionProgress is the resume cursor for one bitrate rendition.
type RenditionProgress struct {
	// NextSegmentIndex is the first segment index not yet durably committed.
	NextSegmentIndex int `json:"next_segment_index"`
	// CommittedHashes maps committed segment index to its output SHA-256 so a
	// resume run can prove the resumed stream reproduces identical bytes.
	CommittedHashes map[int]string `json:"committed_hashes"`
	// LastTick is the extended media tick of the last committed boundary.
	LastTick uint64 `json:"last_tick"`
	// LastRTPSeq is the extended RTP sequence number consumed up to.
	LastRTPSeq uint32 `json:"last_rtp_seq"`
}

// Document is the full checkpoint payload (checksum excluded).
type Document struct {
	Seed        int64                          `json:"seed"`
	ProgramSecs int                            `json:"program_seconds"`
	Renditions  map[string]*RenditionProgress  `json:"renditions"`
}

// envelope wraps the document with its integrity checksum.
type envelope struct {
	Body     Document `json:"body"`
	Checksum string   `json:"checksum"`
}

// Store durably persists one checkpoint document.
type Store struct {
	path   string
	secret []byte
	doc    Document
}

// Open loads an existing checkpoint at path or initializes an empty one. The
// secret binds checksums to a run; pass a stable value derived from the seed.
func Open(path string, secret []byte) (*Store, error) {
	s := &Store{path: path, secret: append([]byte(nil), secret...)}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		s.doc = Document{Renditions: map[string]*RenditionProgress{}}
		return s, nil
	}
	if err != nil {
		return nil, timeline.NewError(timeline.ClassRetryable, "checkpoint.open",
			"cannot read checkpoint", err)
	}
	var env envelope
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		return nil, timeline.IntegrityError("checkpoint.open",
			"checkpoint is not valid JSON", jerr)
	}
	want := s.compute(env.Body)
	if !hmac.Equal([]byte(want), []byte(env.Checksum)) {
		return nil, timeline.IntegrityError("checkpoint.open",
			"checkpoint checksum mismatch (tampered or wrong secret)", nil)
	}
	if env.Body.Renditions == nil {
		env.Body.Renditions = map[string]*RenditionProgress{}
	}
	s.doc = env.Body
	return s, nil
}

// compute returns the hex checksum of a document under the store secret.
func (s *Store) compute(d Document) string {
	canonical, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil))
}

// Progress returns the cursor for a rendition, creating an empty one on first
// use. The returned pointer is the live cursor; callers mutate it then call
// Commit to persist.
func (s *Store) Progress(rendition string) *RenditionProgress {
	p, ok := s.doc.Renditions[rendition]
	if !ok {
		p = &RenditionProgress{CommittedHashes: map[int]string{}}
		s.doc.Renditions[rendition] = p
	}
	if p.CommittedHashes == nil {
		p.CommittedHashes = map[int]string{}
	}
	return p
}

// CommitSegment records a durably committed segment and persists the whole
// checkpoint atomically. It refuses to re-commit an index with a different
// hash, which would indicate a non-deterministic or replayed run.
func (s *Store) CommitSegment(rendition string, index int, outHash string, tick uint64, rtpSeq uint32) error {
	p := s.Progress(rendition)
	if existing, ok := p.CommittedHashes[index]; ok {
		if existing != outHash {
			return timeline.IntegrityError("checkpoint.commit",
				"segment index re-committed with a different hash", nil)
		}
		// Idempotent re-commit of identical content is allowed but not advanced.
		return nil
	}
	p.CommittedHashes[index] = outHash
	if index >= p.NextSegmentIndex {
		p.NextSegmentIndex = index + 1
	}
	p.LastTick = tick
	p.LastRTPSeq = rtpSeq
	return s.persist()
}

// NextIndex returns the first uncommitted segment index for a rendition.
func (s *Store) NextIndex(rendition string) int {
	return s.Progress(rendition).NextSegmentIndex
}

// HashFor returns the committed hash for an index, or "" if not committed.
func (s *Store) HashFor(rendition string, index int) string {
	return s.Progress(rendition).CommittedHashes[index]
}

// Snapshot returns a copy of the current document for reporting.
func (s *Store) Snapshot() Document {
	out := Document{
		Seed:        s.doc.Seed,
		ProgramSecs: s.doc.ProgramSecs,
		Renditions:  make(map[string]*RenditionProgress, len(s.doc.Renditions)),
	}
	for k, v := range s.doc.Renditions {
		cp := *v
		cp.CommittedHashes = make(map[int]string, len(v.CommittedHashes))
		for i, h := range v.CommittedHashes {
			cp.CommittedHashes[i] = h
		}
		out.Renditions[k] = &cp
	}
	return out
}

// SetMeta records run-level metadata used to validate a resume matches the
// same generated program.
func (s *Store) SetMeta(seed int64, programSecs int) {
	s.doc.Seed = seed
	s.doc.ProgramSecs = programSecs
}

// persist writes the checksummed envelope to a temp file, fsyncs and renames.
func (s *Store) persist() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return timeline.NewError(timeline.ClassRetryable, "checkpoint.write",
			"cannot create checkpoint directory", err)
	}
	env := envelope{Body: s.doc, Checksum: s.compute(s.doc)}
	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return timeline.NewError(timeline.ClassRetryable, "checkpoint.write",
			"cannot encode checkpoint", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".ckpt-*")
	if err != nil {
		return timeline.NewError(timeline.ClassRetryable, "checkpoint.write",
			"cannot create temp checkpoint", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return timeline.NewError(timeline.ClassRetryable, "checkpoint.write",
			"write failed", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return timeline.NewError(timeline.ClassRetryable, "checkpoint.write",
			"fsync failed", err)
	}
	if err := tmp.Close(); err != nil {
		return timeline.NewError(timeline.ClassRetryable, "checkpoint.write",
			"close failed", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return timeline.NewError(timeline.ClassRetryable, "checkpoint.write",
			"rename failed", err)
	}
	return nil
}
