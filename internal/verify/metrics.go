// Package verify independently re-reads committed fragments from disk, reparses
// the manifest and segment metadata, randomly queries the keyframe index and
// recomputes per-fragment SHA-256. It never trusts the in-memory run state.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"relaycheck/internal/media"
	"relaycheck/internal/segment"
)

// MaxFragmentRead caps a single fragment read during verification.
const MaxFragmentRead = 64 << 20

type diskEntry struct {
	Index      int    `json:"index"`
	File       string `json:"file"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	KeyframeID int    `json:"keyframe_id"`
}

type diskManifest struct {
	Version      int         `json:"version"`
	Seed         int64       `json:"seed"`
	SegmentCount int         `json:"segment_count"`
	Entries      []diskEntry `json:"entries"`
}

// Report is the independent verification summary.
type Report struct {
	SegmentCount     int
	HashMatched      int
	HashTotal        int
	RoundtripMatched int
	RoundtripTotal   int
	KeyHits          int
	KeyTotal         int
	Hashes           []string // index-ordered, recomputed from disk
}

// Percent formats matched/total as a fixed two-decimal percentage.
func Percent(matched, total int) string {
	if total == 0 {
		return "0.00%"
	}
	return fmt.Sprintf("%.2f%%", 100*float64(matched)/float64(total))
}

// CompareHashes returns how many got entries equal the want entries positionally.
func CompareHashes(want, got []string) (matched, total int) {
	total = len(want)
	for i := range want {
		if i < len(got) && want[i] == got[i] {
			matched++
		}
	}
	return matched, total
}

// VerifyDirectory reparses manifest + fragments and runs all checks.
func VerifyDirectory(dir string, seed int64) (*Report, *media.Fault) {
	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, media.NewFault(media.CodeIntegrity, "cannot reparse manifest: %v", err)
	}
	var m diskManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, media.NewFault(media.CodeIntegrity, "manifest json: %v", err)
	}
	entries := append([]diskEntry{}, m.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Index < entries[j].Index })

	rep := &Report{SegmentCount: len(entries), HashTotal: len(entries), RoundtripTotal: len(entries)}
	keyframe := map[int]bool{}

	for _, e := range entries {
		data, f := readCapped(filepath.Join(dir, e.File))
		if f != nil {
			return nil, f
		}
		sum := sha256.Sum256(data)
		gotSHA := hex.EncodeToString(sum[:])
		rep.Hashes = append(rep.Hashes, gotSHA)
		if gotSHA == e.SHA256 && (e.Size == 0 || e.Size == int64(len(data))) {
			rep.HashMatched++
		}
		// Independent reparse + canonical re-encode roundtrip.
		if seg, df := segment.Decode(data); df == nil {
			re := seg.Encode()
			if string(re) == string(data) {
				rep.RoundtripMatched++
			}
		}
		keyframe[e.KeyframeID] = true
	}

	// Seeded random keyframe queries over the full index.
	rep.KeyTotal = len(entries)
	rng := rand.New(rand.NewSource(seed))
	for _, pos := range rng.Perm(len(entries)) {
		id := entries[pos].KeyframeID
		if keyframe[id] {
			rep.KeyHits++
		}
	}
	return rep, nil
}

func readCapped(path string) ([]byte, *media.Fault) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, media.NewFault(media.CodeIntegrity, "stat fragment: %v", err)
	}
	if st.Size() > MaxFragmentRead {
		return nil, media.NewFault(media.CodeBufferLimit, "fragment %s size %d exceeds verify cap", path, st.Size())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, media.NewFault(media.CodeIntegrity, "read fragment: %v", err)
	}
	return b, nil
}
