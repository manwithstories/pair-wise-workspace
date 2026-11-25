// Package audit produces round-trip field consistency statistics, per-shard
// SHA-256 digests and the SHA256SUMS manifest used to prove that repeated runs
// are byte-identical.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"relay.local/relay/internal/model"
)

// FieldStat counts field agreements between an original and a rewritten
// description.
type FieldStat struct {
	Total   int
	Matched int
}

// Rate returns Matched/Total, or 1 when nothing was compared.
func (f FieldStat) Rate() float64 {
	if f.Total == 0 {
		return 1
	}
	return float64(f.Matched) / float64(f.Total)
}

// Roundtrip compares two field tables (as produced by model.Track.FieldTable)
// and returns the exact agreement rate over the fields present in both. Fields
// missing from the rewritten table count as mismatches.
type Roundtrip struct {
	Compared int
	Matched  int
	Missing  []string
	Differs  []string
}

// Rate returns Matched/Compared; a run with no fields is treated as perfect.
func (r Roundtrip) Rate() float64 {
	if r.Compared == 0 {
		return 1
	}
	return float64(r.Matched) / float64(r.Compared)
}

// String renders "matched/compared".
func (r Roundtrip) String() string {
	return fmt.Sprintf("%d/%d", r.Matched, r.Compared)
}

// CompareFields diffs two ordered "key=value" tables.
func CompareFields(original, rewritten []string) Roundtrip {
	orig := make(map[string]string, len(original))
	for _, line := range original {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		orig[k] = v
	}
	rew := make(map[string]string, len(rewritten))
	for _, line := range rewritten {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		rew[k] = v
	}
	keys := make([]string, 0, len(orig))
	for k := range orig {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var rt Roundtrip
	for _, k := range keys {
		rt.Compared++
		got, ok := rew[k]
		switch {
		case !ok:
			rt.Missing = append(rt.Missing, k)
		case got == orig[k]:
			rt.Matched++
		default:
			rt.Differs = append(rt.Differs, k)
		}
	}
	return rt
}

// HashBytes returns the lowercase hex SHA-256 of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Shard is one digest region of a stream.
type Shard struct {
	Index int
	Hash  string
	Bytes int64
}

// HashShards splits data into n contiguous shards and hashes each. Shards are
// deterministic: identical input and n always produce identical digests.
func HashShards(data []byte, n int) []Shard {
	if n <= 0 {
		n = 1
	}
	if n > len(data) {
		n = len(data)
	}
	if n == 0 {
		return nil
	}
	shards := make([]Shard, 0, n)
	size := len(data) / n
	extra := len(data) % n
	pos := 0
	for i := 0; i < n; i++ {
		end := pos + size
		if i < extra {
			end++
		}
		shards = append(shards, Shard{Index: i, Hash: HashBytes(data[pos:end]), Bytes: int64(end - pos)})
		pos = end
	}
	return shards
}

// HashFiles hashes the concatenation of the named files in the given order.
func HashFiles(paths []string) (string, error) {
	h := sha256.New()
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("audit: read %s: %w", p, err)
		}
		if _, err := h.Write(data); err != nil {
			return "", fmt.Errorf("audit: hash %s: %w", p, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ManifestLine is one SHA256SUMS entry.
type ManifestLine struct {
	Hash string
	Name string
}

// BuildManifest hashes every file in dir (non-recursively) matching the
// predicate, sorted by name, and returns the manifest body.
func BuildManifest(dir string, include func(name string) bool) ([]ManifestLine, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("audit: read %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if include != nil && !include(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	out := make([]ManifestLine, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("audit: read %s: %w", name, err)
		}
		out = append(out, ManifestLine{Hash: HashBytes(data), Name: name})
	}
	return out, nil
}

// WriteManifest writes lines in the sha256sum(1) "<hash>  <name>" format.
func WriteManifest(path string, lines []ManifestLine) error {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Hash)
		b.WriteString("  ")
		b.WriteString(l.Name)
		b.WriteByte('\n')
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("audit: write manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("audit: publish manifest: %w", err)
	}
	return nil
}

// ReadManifest parses a manifest written by WriteManifest.
func ReadManifest(path string) ([]ManifestLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []ManifestLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 || len(parts[0]) != 64 {
			return nil, fmt.Errorf("audit: malformed manifest line %q", line)
		}
		out = append(out, ManifestLine{Hash: parts[0], Name: parts[1]})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("audit: read manifest: %w", err)
	}
	return out, nil
}

// CompareManifests returns the names whose digests differ, plus names present
// in only one side. An empty result means the manifests are identical.
func CompareManifests(a, b []ManifestLine) []string {
	am := map[string]string{}
	bm := map[string]string{}
	for _, l := range a {
		am[l.Name] = l.Hash
	}
	for _, l := range b {
		bm[l.Name] = l.Hash
	}
	names := map[string]bool{}
	for n := range am {
		names[n] = true
	}
	for n := range bm {
		names[n] = true
	}
	var diff []string
	for n := range names {
		if am[n] != bm[n] {
			diff = append(diff, n)
		}
	}
	sort.Strings(diff)
	return diff
}

// Segment describes one contiguous range of composed output.
type Segment struct {
	Index int
	First int
	Last  int
	Hash  string
}

// SegmentHash computes the digest of a segment from the per-frame hashes,
// excluding the frame name so the digest depends only on pixel content.
func SegmentHash(frameHashes []string) string {
	h := sha256.New()
	for _, fh := range frameHashes {
		h.Write([]byte(fh))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SplitSegments partitions n frames into at most segments contiguous segments
// and returns each segment's digest.
func SplitSegments(frameHashes []string, segments int) []Segment {
	if segments <= 0 || len(frameHashes) == 0 {
		return nil
	}
	if segments > len(frameHashes) {
		segments = len(frameHashes)
	}
	out := make([]Segment, 0, segments)
	size := len(frameHashes) / segments
	extra := len(frameHashes) % segments
	pos := 0
	for i := 0; i < segments; i++ {
		end := pos + size
		if i < extra {
			end++
		}
		out = append(out, Segment{
			Index: i, First: pos, Last: end - 1,
			Hash: SegmentHash(frameHashes[pos:end]),
		})
		pos = end
	}
	return out
}

// TrackDigest hashes the field table of a track for cross-run comparison.
func TrackDigest(t *model.Track) string {
	h := sha256.New()
	for _, line := range t.FieldTable() {
		h.Write([]byte(line))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FormatRate renders a rate as a three-decimal string such as "1.000".
func FormatRate(matched, total int) string {
	if total == 0 {
		return "1.000"
	}
	// Integer math keeps the output identical across platforms.
	permille := (int64(matched)*1000 + int64(total)/2) / int64(total)
	return strconv.FormatInt(permille/1000, 10) + "." + fmt.Sprintf("%03d", permille%1000)
}
