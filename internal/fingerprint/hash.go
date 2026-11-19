// Package fingerprint computes deterministic, camera-independent content
// hashes for clips. Hashing refuses to run when timebases differ or the data
// is incomplete, so near-duplicates are never reported silently.
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"

	"mediasync/internal/media"
	"mediasync/internal/timeline"
)

// Version is bumped whenever the hashing recipe changes.
const Version = 1

// Flags describe data-quality conditions that affect trust in a hash.
type Flags struct {
	Timebase      *timeline.Timebase `json:"timebase,omitempty"`
	Incomplete    bool               `json:"incomplete"`
	MissingRanges []string           `json:"missing_ranges,omitempty"`
	SampleCount   int                `json:"sample_count"`
	ExpectedCount int                `json:"expected_count"`
}

// Compatible explains why two flag sets cannot be compared.
func (f Flags) Incompatible(other Flags) string {
	if f.Timebase == nil || other.Timebase == nil {
		return "missing timebase"
	}
	if !f.Timebase.Compatible(*other.Timebase) {
		return fmt.Sprintf("timebase %s/%s vs %s/%s", f.Timebase.Rate, f.Timebase.Clock.Domain,
			other.Timebase.Rate, other.Timebase.Clock.Domain)
	}
	if f.Incomplete || other.Incomplete {
		return "incomplete data"
	}
	return ""
}

// Description is the canonical, order-stable byte sequence hashed for a clip.
// Only content identity is included: no file names, offsets or paths, so two
// cameras covering the same material hash identically.
type Description struct {
	StartNS     int64
	EndNS       int64
	Samples     []SampleDigest
	Transcript  string
	SilenceMask []int64
}

// SampleDigest stands for one sample's content, typically the SHA-256 of its
// bytes truncated to 16 bytes.
type SampleDigest struct {
	DTSNS      int64
	DurationNS int64
	Size       uint32
	Digest     [16]byte
}

// Hash returns the deterministic SHA-256 of a description as hex.
func (d Description) Hash() (string, error) {
	h := sha256.New()
	writeHeader(h, d, Version)
	for _, s := range d.Samples {
		var buf [8 + 8 + 4 + 16]byte
		binary.BigEndian.PutUint64(buf[0:8], uint64(s.DTSNS))
		binary.BigEndian.PutUint64(buf[8:16], uint64(s.DurationNS))
		binary.BigEndian.PutUint32(buf[16:20], s.Size)
		copy(buf[20:], s.Digest[:])
		h.Write(buf[:])
	}
	writeTail(h, d)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Short returns the first 16 hex characters of a hash.
func Short(hash string) string {
	if len(hash) <= 16 {
		return hash
	}
	return hash[:16]
}

func writeHeader(h hash.Hash, d Description, version int) {
	var buf [8 + 8 + 4]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(d.StartNS))
	binary.BigEndian.PutUint64(buf[8:16], uint64(d.EndNS))
	binary.BigEndian.PutUint32(buf[16:20], uint32(version))
	h.Write(buf[:])
	binary.Write(h, binary.BigEndian, uint64(len(d.Samples)))
}

func writeTail(h hash.Hash, d Description) {
	blob := make([]byte, 0, len(d.Transcript)+8*len(d.SilenceMask))
	blob = append(blob, d.Transcript...)
	binary.Write(h, binary.BigEndian, uint64(len(d.SilenceMask)))
	for _, v := range d.SilenceMask {
		binary.Write(h, binary.BigEndian, uint64(v))
	}
	h.Write(blob)
}

// DigestOfBytes returns the truncated content digest used by SampleDigest.
func DigestOfBytes(b []byte) [16]byte {
	sum := sha256.Sum256(b)
	var out [16]byte
	copy(out[:], sum[:16])
	return out
}

// Match is one pair of clips with identical content hashes.
type Match struct {
	Hash     string   `json:"hash"`
	Short    string   `json:"short"`
	Cameras  []string `json:"cameras"`
	Segments []int    `json:"segments"`
}

// Compare groups clips by hash and reports cross-camera duplicates. Clips
// whose data is incomplete are excluded. Hash groups are additionally split by
// timebase compatibility: a clip that cannot be converted onto the same
// timeline as the group's first clip is never silently matched, it is reported
// as incompatible instead.
func Compare(clips []Clip) ([]Match, []*media.Issue) {
	var issues []*media.Issue
	groups := map[string][]Clip{}
	for i, c := range clips {
		if c.Hash == "" {
			issues = append(issues, media.New(media.StageFingerprint, media.InputMP4, media.CodeIncomplete,
				"clip %s#%d has no hash", c.Camera, c.Index).WithRecord(i+1))
			continue
		}
		if c.Flags.Incomplete {
			issues = append(issues, media.New(media.StageFingerprint, media.InputMP4, media.CodeIncomplete,
				"clip %s#%d excluded: %s", c.Camera, c.Index, c.Flags.IncompleteReason()).WithRecord(i+1))
			continue
		}
		groups[c.Hash] = append(groups[c.Hash], c)
	}
	for hash, group := range groups {
		ref := group[0].Flags
		kept := []Clip{group[0]}
		for _, c := range group[1:] {
			if reason := ref.Incompatible(c.Flags); reason != "" {
				issues = append(issues, media.New(media.StageFingerprint, media.InputMP4, media.CodeIncompatible,
					"clip %s#%d (group %s) not compared with %s#%d: %s",
					c.Camera, c.Index, Short(hash), group[0].Camera, group[0].Index, reason).
					WithRecord(c.Index+1))
				continue
			}
			kept = append(kept, c)
		}
		groups[hash] = kept
	}
	hashes := make([]string, 0, len(groups))
	for h := range groups {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	var matches []Match
	for _, h := range hashes {
		g := groups[h]
		if len(g) < 2 {
			continue
		}
		cams := map[string]struct{}{}
		m := Match{Hash: h, Short: Short(h)}
		for _, c := range g {
			if _, ok := cams[c.Camera]; !ok {
				cams[c.Camera] = struct{}{}
				m.Cameras = append(m.Cameras, c.Camera)
			}
			m.Segments = append(m.Segments, c.Index)
		}
		sort.Strings(m.Cameras)
		sort.Ints(m.Segments)
		matches = append(matches, m)
	}
	return matches, issues
}

// HashCompatibility reports whether two clips may be compared, returning a
// reason when they may not.
func HashCompatibility(a, b Flags) string { return a.Incompatible(b) }
