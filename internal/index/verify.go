package index

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"bcast/internal/media"
)

// IndexFile is the JSON sidecar committed alongside binary indices.
type IndexFile struct {
	PrimaryTrackID uint32      `json:"primary_track_id"`
	Timescale      int64       `json:"timescale"`
	Entries        []SeekEntry `json:"entries"`
	Manifest       string      `json:"manifest"`
	SIDXFile       string      `json:"sidx_file"`
	TFRAFile       string      `json:"tfra_file"`
}

// VerifyResult reports random-access and playlist timing verification.
type VerifyResult struct {
	SeekOK       int     `json:"seek_ok"`
	SeekTotal    int     `json:"seek_total"`
	ManifestSkew float64 `json:"manifest_skew_ms"`
}

func buildIndexJSON(a *Artifacts) ([]byte, error) {
	doc := IndexFile{
		PrimaryTrackID: a.PrimaryTrackID,
		Timescale:      media.SharedTimescale,
		Entries:        a.Entries,
		Manifest:       a.ManifestPath,
		SIDXFile:       "index.sidx",
		TFRAFile:       "index.tfra",
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, media.Corrupt("index", "index_json", "", 0, -1, "%v", err)
	}
	return append(b, '\n'), nil
}

// Verify checks every indexed moof offset and compares every EXTINF value with
// the repaired fragment duration.  sourceDir is the directory containing the
// generated .m4s files named in the manifest.
func Verify(sourceDir string, a *Artifacts) (*VerifyResult, error) {
	res := &VerifyResult{SeekTotal: len(a.Entries)}
	for _, e := range a.Entries {
		path := filepath.Join(sourceDir, e.Segment)
		f, err := os.Open(path)
		if err != nil {
			return res, media.IO("verify", path, e.Fragment, e.MoofOffset, err)
		}
		buf := make([]byte, 8)
		_, err = f.ReadAt(buf, e.MoofOffset)
		_ = f.Close()
		if err != nil {
			return res, media.Corrupt("verify", "seek_offset_unreadable", path, e.Fragment, e.MoofOffset, "%v", err)
		}
		if string(buf[4:8]) != "moof" {
			return res, media.Corrupt("verify", "seek_point_not_moof", path, e.Fragment, e.MoofOffset,
				"box type %q", string(buf[4:8]))
		}
		res.SeekOK++
	}

	skew, err := verifyManifestDurations(a)
	if err != nil {
		return res, err
	}
	res.ManifestSkew = skew
	if skew > 2.0 {
		return res, media.Corrupt("verify", "manifest_skew_exceeds_2ms", "manifest.m3u8", 0, -1,
			"maximum skew %.6f ms", skew)
	}
	return res, nil
}

func verifyManifestDurations(a *Artifacts) (float64, error) {
	expected := map[string]float64{}
	for _, e := range a.Entries {
		// Entries are sorted by time; equal segment values overwrite identically.
		expected[e.Segment] = media.Timebase().Seconds(e.DurationShared)
	}
	lines := strings.Split(string(a.Manifest), "\n")
	var extinf float64
	var haveExtinf bool
	maxSkew := 0.0
	matched := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXTINF:") {
			v := strings.TrimPrefix(line, "#EXTINF:")
			v = strings.TrimSuffix(v, ",")
			d, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return 0, media.Corrupt("verify", "invalid_extinf", "manifest.m3u8", 0, -1, "%v", err)
			}
			extinf, haveExtinf = d, true
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		want, ok := expected[line]
		if !ok {
			return 0, media.Corrupt("verify", "manifest_unknown_segment", "manifest.m3u8", 0, -1, line)
		}
		if !haveExtinf {
			return 0, media.Corrupt("verify", "missing_extinf", "manifest.m3u8", 0, -1, line)
		}
		skew := math.Abs(extinf-want) * 1000
		if skew > maxSkew {
			maxSkew = skew
		}
		matched++
		haveExtinf = false
	}
	if matched != len(expected) {
		return maxSkew, media.Corrupt("verify", "manifest_segment_count", "manifest.m3u8", 0, -1,
			"manifest lists %d segments, index has %d", matched, len(expected))
	}
	return math.Round(maxSkew*1_000_000) / 1_000_000, nil
}

// Filename constants used by the transaction committer.
const (
	ManifestFile  = "manifest.m3u8"
	IndexJSONFile = "index.json"
	SIDXFile      = "index.sidx"
	TFRAFile      = "index.tfra"
	ReportFile    = "report.json"
)

// String implements a concise diagnostic.
func (r *VerifyResult) String() string {
	return fmt.Sprintf("seek_ok=%d/%d manifest_skew_ms=%.3f", r.SeekOK, r.SeekTotal, r.ManifestSkew)
}
