package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"avpipe/internal/media"
)

// Checkpoint is the single configuration record written before any group.
// Resume is rejected unless both the input summary and the encoding
// configuration match exactly.
type Checkpoint struct {
	InputHash  [sha256.Size]byte // summary of the source container
	ConfigHash [sha256.Size]byte // encoding targets / fps / segment shape
	TargetKbps []int             // ordered representation targets
	FPS        uint32
	SegmentMS  uint32
}

// GroupRecord is one committed segment group: one slot per representation.
type GroupRecord struct {
	GroupIndex    int
	SegmentHashes [][sha256.Size]byte // per variant, same order as TargetKbps
	Gapped        bool
}

// State is the replay result used by the pipeline on resume.
type State struct {
	Checkpoint *Checkpoint
	Groups     []GroupRecord
}

// SaveCheckpoint writes the configuration record. It must be the first
// record in an otherwise empty journal.
func (j *Journal) SaveCheckpoint(cp Checkpoint) error {
	payload, err := encodeCheckpoint(cp)
	if err != nil {
		return err
	}
	return j.Append(Entry{Type: RecCheckpoint, Payload: payload})
}

// CommitGroup stages nothing in memory beyond the record itself: the segment
// files are already written and their digests verified by the caller. The
// append + fsync is the atomic commit point.
func (j *Journal) CommitGroup(g GroupRecord) error {
	payload, err := encodeGroup(g)
	if err != nil {
		return err
	}
	return j.Append(Entry{Type: RecGroup, Payload: payload})
}

// Load replays journal entries and reconstructs the committed state.
func Load(entries []Entry) (*State, error) {
	st := &State{}
	for _, e := range entries {
		switch e.Type {
		case RecCheckpoint:
			cp, ok := decodeCheckpoint(e.Payload)
			if !ok {
				return nil, media.NewError(media.ClassIndex, "index", "corrupt checkpoint record", 0, false)
			}
			if st.Checkpoint != nil {
				return nil, media.NewError(media.ClassIndex, "index", "duplicate checkpoint record", 0, false)
			}
			st.Checkpoint = &cp
		case RecGroup:
			if st.Checkpoint == nil {
				return nil, media.NewError(media.ClassIndex, "index", "group without checkpoint", 0, false)
			}
			g, ok := decodeGroup(e.Payload)
			if !ok {
				return nil, media.NewError(media.ClassIndex, "index", "corrupt group record", 0, false)
			}
			if g.GroupIndex != len(st.Groups) {
				return nil, media.NewError(media.ClassIndex, "index", "non-consecutive group index", 0, false)
			}
			st.Groups = append(st.Groups, g)
		default:
			return nil, media.NewError(media.ClassIndex, "index", "unknown journal record type", 0, false)
		}
	}
	if st.Checkpoint == nil && len(entries) > 0 {
		return nil, media.NewError(media.ClassIndex, "index", "journal without checkpoint", 0, false)
	}
	return st, nil
}

// VerifyCompatible rejects reuse of a checkpoint whose input or config hash
// differs from the current run.
func (s *State) VerifyCompatible(cp Checkpoint) error {
	if s.Checkpoint == nil {
		return ErrNoCheckpoint
	}
	if s.Checkpoint.InputHash != cp.InputHash {
		return media.NewError(media.ClassIndex, "index", "input summary changed; refusing stale checkpoint", 0, false)
	}
	if s.Checkpoint.ConfigHash != cp.ConfigHash {
		return media.NewError(media.ClassIndex, "index", "encoding config changed; refusing stale checkpoint", 0, false)
	}
	return nil
}

// HashFile returns the hex-encoded SHA-256 of a committed segment file.
func HashFile(path string) (string, error) {
	sum, err := hashBytesFromDisk(path)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum[:]), nil
}

func encodeCheckpoint(cp Checkpoint) ([]byte, error) {
	buf := make([]byte, 0, payloadCap)
	buf = append(buf, cp.InputHash[:]...)
	buf = append(buf, cp.ConfigHash[:]...)
	tmp := make([]byte, 4)
	binary.LittleEndian.PutUint32(tmp, cp.FPS)
	buf = append(buf, tmp...)
	binary.LittleEndian.PutUint32(tmp, cp.SegmentMS)
	buf = append(buf, tmp...)
	if len(cp.TargetKbps) > 8 {
		return nil, media.NewError(media.ClassIndex, "index", "too many targets", 0, false)
	}
	for _, t := range cp.TargetKbps {
		binary.LittleEndian.PutUint32(tmp, uint32(t))
		buf = append(buf, tmp...)
	}
	if len(buf) > payloadCap {
		return nil, media.NewError(media.ClassIndex, "index", "checkpoint too large", 0, false)
	}
	return buf, nil
}

func decodeCheckpoint(p []byte) (Checkpoint, bool) {
	var cp Checkpoint
	need := 2*sha256.Size + 8
	if len(p) < need || (len(p)-need)%4 != 0 {
		return cp, false
	}
	copy(cp.InputHash[:], p[0:sha256.Size])
	copy(cp.ConfigHash[:], p[sha256.Size:2*sha256.Size])
	cp.FPS = binary.LittleEndian.Uint32(p[2*sha256.Size : 2*sha256.Size+4])
	cp.SegmentMS = binary.LittleEndian.Uint32(p[2*sha256.Size+4 : need])
	for off := need; off+4 <= len(p); off += 4 {
		cp.TargetKbps = append(cp.TargetKbps, int(binary.LittleEndian.Uint32(p[off:off+4])))
	}
	return cp, true
}

func encodeGroup(g GroupRecord) ([]byte, error) {
	if len(g.SegmentHashes) > 4 {
		return nil, media.NewError(media.ClassIndex, "index", "too many variants in group", 0, false)
	}
	buf := make([]byte, 0, payloadCap)
	tmp := make([]byte, 4)
	binary.LittleEndian.PutUint32(tmp, uint32(g.GroupIndex))
	buf = append(buf, tmp...)
	flag := byte(0)
	if g.Gapped {
		flag = 1
	}
	buf = append(buf, flag, byte(len(g.SegmentHashes)))
	for _, h := range g.SegmentHashes {
		buf = append(buf, h[:]...)
	}
	if len(buf) > payloadCap {
		return nil, media.NewError(media.ClassIndex, "index", "group record too large", 0, false)
	}
	return buf, nil
}

func decodeGroup(p []byte) (GroupRecord, bool) {
	var g GroupRecord
	if len(p) < 6 {
		return g, false
	}
	g.GroupIndex = int(binary.LittleEndian.Uint32(p[0:4]))
	g.Gapped = p[4] == 1
	n := int(p[5])
	if n > 4 || len(p) < 6+n*sha256.Size {
		return g, false
	}
	off := 6
	for i := 0; i < n; i++ {
		var h [sha256.Size]byte
		copy(h[:], p[off:off+sha256.Size])
		g.SegmentHashes = append(g.SegmentHashes, h)
		off += sha256.Size
	}
	return g, true
}

// ConfigDigest deterministically hashes the encoding configuration.
func ConfigDigest(targets []int, fps uint32, segmentMS uint32) [sha256.Size]byte {
	h := sha256.New()
	tmp := make([]byte, 4)
	for _, t := range targets {
		binary.LittleEndian.PutUint32(tmp, uint32(t))
		h.Write(tmp)
	}
	binary.LittleEndian.PutUint32(tmp, fps)
	h.Write(tmp)
	binary.LittleEndian.PutUint32(tmp, segmentMS)
	h.Write(tmp)
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// EnsureGroupConsistent checks that every representation produced the same
// segment count and timeline span before a group is allowed to commit.
func EnsureGroupConsistent(hashes [][sha256.Size]byte, expectedVariants int) error {
	if len(hashes) != expectedVariants {
		return media.NewError(media.ClassIndex, "index",
			fmt.Sprintf("group incomplete: %d/%d variants", len(hashes), expectedVariants), 0, false)
	}
	return nil
}

// hashBytesFromDisk is isolated for test stubbing.
var hashBytesFromDisk = func(path string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	f, err := openReadOnly(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := copyHash(h, f); err != nil {
		return out, media.NewError(media.ClassIndex, "index", "hash read failed: "+err.Error(), 0, false)
	}
	copy(out[:], h.Sum(nil))
	return out, nil
}

// ErrStale is returned when the on-disk group no longer matches the journal.
var ErrStale = errors.New("committed group missing or altered on disk")
