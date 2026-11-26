// Package index builds and persists the scan index: the unified
// rational-second timeline of all segments plus the shared timeline
// error ranges. The index is built only during the scan phase; the
// execution phase streams segments according to it. A stored index is
// invalidated when the input digest or the analysis config changes.
package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediascan/internal/core"
	"mediascan/internal/manifest"
	"mediascan/internal/segment"
)

// Input limits (over-limit input is rejected, never silently truncated).
const (
	MaxManifestSize = 1 << 20
	MaxSegmentSize  = 64 << 20
	maxManifests    = 64
	maxSegments     = 4096
	hlsTimescale    = 90000
)

// Config captures the analysis settings that affect index contents.
type Config struct {
	Hashes bool `json:"hashes"`
}

// Digest returns the canonical config fingerprint.
func (c Config) Digest() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SegmentRef is one indexed segment on the unified timeline.
type SegmentRef struct {
	ID           string   `json:"id"`
	Path         string   `json:"path"` // slash-separated, relative to input root
	Source       string   `json:"source"`
	RepID        string   `json:"rep_id,omitempty"`
	Start        core.Rat `json:"start"`
	End          core.Rat `json:"end"`
	DiscSeq      int      `json:"disc_seq"`
	Timescale    int64    `json:"timescale"`
	CheckTfdt    bool     `json:"check_tfdt"`
	ExpectedTfdt uint64   `json:"expected_tfdt"`
}

// Index is the scan-phase artifact.
type Index struct {
	InputDigest    string       `json:"input_digest"`
	ConfigDigest   string       `json:"config_digest"`
	RoundTripOK    int          `json:"roundtrip_ok"`
	RoundTripTotal int          `json:"roundtrip_total"`
	Segments       []SegmentRef `json:"segments"`
	TimelineErrors []core.Range `json:"timeline_errors"`
}

// ReadLimited reads a file, rejecting inputs above the size cap.
func ReadLimited(path string, max int64) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("file exceeds size limit: %s (%d > %d bytes)", path, fi.Size(), max)
	}
	return os.ReadFile(path)
}

// SafeJoin resolves rel against root, rejecting remote URLs and any
// path that would escape the input root.
func SafeJoin(root, rel string) (string, error) {
	if strings.Contains(rel, "://") || strings.HasPrefix(rel, "//") {
		return "", fmt.Errorf("remote address rejected: %q", rel)
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes input root: %q", rel)
	}
	return filepath.Join(root, clean), nil
}

// HashInput fingerprints every regular file under root (sorted by path).
func HashInput(root string) (string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("non-regular file rejected: %s", p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		p, err := SafeJoin(root, rel)
		if err != nil {
			return "", err
		}
		data, err := ReadLimited(p, MaxSegmentSize)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(h, "%s\n%x\n", rel, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type manifestFile struct {
	path string // relative to root, slash form
	kind string // "hls-master", "hls-media", "dash"
}

func discover(root string) ([]manifestFile, error) {
	var out []manifestFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch strings.ToLower(filepath.Ext(p)) {
		case ".m3u8":
			data, err := ReadLimited(p, MaxManifestSize)
			if err != nil {
				return err
			}
			kind := "hls-media"
			if bytes.Contains(data, []byte("#EXT-X-STREAM-INF")) {
				kind = "hls-master"
			}
			out = append(out, manifestFile{rel, kind})
		case ".mpd":
			out = append(out, manifestFile{rel, "dash"})
		}
		if len(out) > maxManifests {
			return fmt.Errorf("too many manifests (>%d)", maxManifests)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// Build constructs the index from the input tree. Manifest parse or
// round-trip failures abort the build (input error); segment-level
// problems are left for the execution phase.
func Build(root string, cfg Config) (*Index, error) {
	digest, err := HashInput(root)
	if err != nil {
		return nil, err
	}
	idx := &Index{InputDigest: digest, ConfigDigest: cfg.Digest()}
	manifests, err := discover(root)
	if err != nil {
		return nil, err
	}
	if len(manifests) == 0 {
		return nil, fmt.Errorf("no manifests found under %s", root)
	}
	for _, m := range manifests {
		p, err := SafeJoin(root, m.path)
		if err != nil {
			return nil, err
		}
		data, err := ReadLimited(p, MaxManifestSize)
		if err != nil {
			return nil, err
		}
		idx.RoundTripTotal++
		switch m.kind {
		case "hls-master":
			if _, err := manifest.RoundTripHLSMaster(m.path, data); err != nil {
				return nil, err
			}
		case "hls-media":
			mp, err := manifest.RoundTripHLSMedia(m.path, data)
			if err != nil {
				return nil, err
			}
			if err := idx.addHLS(root, m.path, mp); err != nil {
				return nil, err
			}
		case "dash":
			mpd, err := manifest.RoundTripDASH(m.path, data)
			if err != nil {
				return nil, err
			}
			if err := idx.addDASH(root, m.path, mpd); err != nil {
				return nil, err
			}
		}
		idx.RoundTripOK++
	}
	if len(idx.Segments) == 0 {
		return nil, fmt.Errorf("no segments indexed under %s", root)
	}
	if len(idx.Segments) > maxSegments {
		return nil, fmt.Errorf("too many segments (>%d)", maxSegments)
	}
	idx.verifyTfdt(root)
	sort.Slice(idx.Segments, func(i, j int) bool { return idx.Segments[i].Path < idx.Segments[j].Path })
	idx.TimelineErrors = core.MergeRanges(idx.TimelineErrors)
	return idx, nil
}

func (idx *Index) addHLS(root, mpath string, mp *manifest.HLSMedia) error {
	dir := filepath.ToSlash(filepath.Dir(mpath))
	cursor := core.NewRat(0, 1)
	disc := 0
	var prevPDT int64
	var prevDur core.Rat
	prevHasPDT := false
	for _, s := range mp.Segments {
		if s.Discontinuity {
			disc++
		}
		rel := path(dir, s.URI)
		if _, err := SafeJoin(root, rel); err != nil {
			return err
		}
		start, end := cursor, cursor.Add(s.Duration)
		if s.HasPDT && prevHasPDT {
			if actual := core.RatFromMillis(s.PDTUnixMilli - prevPDT); actual != prevDur {
				idx.TimelineErrors = append(idx.TimelineErrors, core.Range{
					Start: start, End: end, Source: "timeline",
					Reason: fmt.Sprintf("program-date-time conflict at %s", rel),
				})
			}
		}
		ref := SegmentRef{
			ID: rel, Path: rel, Source: "hls",
			Start: start, End: end, DiscSeq: disc,
			Timescale: hlsTimescale,
		}
		if t := start.Mul(hlsTimescale); t.Den == 1 && t.Num >= 0 {
			ref.CheckTfdt = true
			ref.ExpectedTfdt = uint64(t.Num)
		}
		idx.Segments = append(idx.Segments, ref)
		prevHasPDT = s.HasPDT
		prevPDT = s.PDTUnixMilli
		prevDur = s.Duration
		cursor = end
	}
	return nil
}

func (idx *Index) addDASH(root, mpath string, mpd *manifest.MPD) error {
	dir := filepath.ToSlash(filepath.Dir(mpath))
	for _, rep := range mpd.Representations {
		for i, s := range rep.Segments {
			rel := path(dir, rep.SegmentURL(i))
			if _, err := SafeJoin(root, rel); err != nil {
				return err
			}
			start := rep.StartSeconds(s)
			end := start.Add(rep.DurationSeconds(s))
			if start.IsNegative() {
				idx.TimelineErrors = append(idx.TimelineErrors, core.Range{
					Start: core.Rat{}, End: end, Source: "timeline",
					Reason: fmt.Sprintf("presentation time below offset at %s", rel),
				})
			}
			idx.Segments = append(idx.Segments, SegmentRef{
				ID: rel, Path: rel, Source: "dash", RepID: rep.ID,
				Start: start, End: end,
				Timescale: int64(rep.Timescale), CheckTfdt: true, ExpectedTfdt: s.T,
			})
		}
	}
	return nil
}

// verifyTfdt reads each segment's tfdt and checks it against the
// manifest-derived expectation and per-track monotonicity. Segments
// that cannot be read or parsed are skipped here; the execution phase
// attributes the damage.
func (idx *Index) verifyTfdt(root string) {
	last := map[string]uint64{}
	for i := range idx.Segments {
		s := &idx.Segments[i]
		if !s.CheckTfdt {
			continue
		}
		p, err := SafeJoin(root, s.Path)
		if err != nil {
			continue
		}
		data, err := ReadLimited(p, MaxSegmentSize)
		if err != nil {
			continue
		}
		frag, err := segment.ParseFragment(data)
		if err != nil {
			continue
		}
		key := s.Source + "/" + s.RepID
		if !frag.HasTfdt {
			idx.TimelineErrors = append(idx.TimelineErrors, core.Range{
				Start: s.Start, End: s.End, Source: "timeline",
				Reason: fmt.Sprintf("missing tfdt at %s", s.Path),
			})
		} else {
			if frag.BaseDecodeTime != s.ExpectedTfdt {
				idx.TimelineErrors = append(idx.TimelineErrors, core.Range{
					Start: s.Start, End: s.End, Source: "timeline",
					Reason: fmt.Sprintf("tfdt %d != expected %d at %s", frag.BaseDecodeTime, s.ExpectedTfdt, s.Path),
				})
			}
			if prev, ok := last[key]; ok && frag.BaseDecodeTime < prev {
				idx.TimelineErrors = append(idx.TimelineErrors, core.Range{
					Start: s.Start, End: s.End, Source: "timeline",
					Reason: fmt.Sprintf("tfdt rollback at %s", s.Path),
				})
			}
			last[key] = frag.BaseDecodeTime
		}
	}
}

// path joins slash-form relative paths.
func path(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return dir + "/" + name
}
