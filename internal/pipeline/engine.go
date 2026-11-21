package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"mediad/internal/audio"
	"mediad/internal/dag"
	"mediad/internal/fingerprint"
	"mediad/internal/manifest"
	"mediad/internal/model"
	"mediad/internal/segment"
	"mediad/internal/store"
	"mediad/internal/verify"
)

// Artifact is the durable per-segment product written by a leaf node.
type Artifact struct {
	Segment model.SegmentRef         `json:"segment"`
	Walk    *segment.WalkReport      `json:"walk,omitempty"`
	Cut     *segment.Cut             `json:"cut,omitempty"`
	FP      *fingerprint.Fingerprint `json:"fp,omitempty"`
	Silence []audio.Interval         `json:"silence,omitempty"`
	Bad     *verify.BadOffset        `json:"bad,omitempty"`
}

// Outcome bundles the final report with DAG results and the exit code.
type Outcome struct {
	Report  *verify.Report
	Results []dag.Result
	Exit    int
}

// Engine runs the full pipeline against one fixture root.
type Engine struct {
	root     string
	outDir   string
	store    *store.FileStore
	detector *audio.Detector
}

// NewEngine wires output directories and the silence detector.
func NewEngine(root, outDir string) (*Engine, error) {
	st, err := store.NewFileStore(filepath.Join(outDir, "checkpoint"))
	if err != nil {
		return nil, err
	}
	det, err := audio.NewDetector(audio.Config{
		SampleRate: sampleRate, Threshold: 50, MinSilenceMS: 300,
	})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(outDir, "artifacts"), 0o755); err != nil {
		return nil, err
	}
	return &Engine{root: root, outDir: outDir, store: st, detector: det}, nil
}

// loadTimelines parses all three manifests and returns confirmed segments in
// a stable (track, index) order plus the partial-timeline flag.
func (e *Engine) loadTimelines() ([]model.SegmentRef, bool, error) {
	fsys := os.DirFS(e.root)
	hlsStudio, err := manifest.ParseHLS(fsys, "studio/playlist.m3u8", model.TrackStudio)
	if err != nil {
		return nil, false, err
	}
	hlsVoice, err := manifest.ParseHLS(fsys, "voice/playlist.m3u8", model.TrackVoice)
	if err != nil {
		return nil, false, err
	}
	dashField, err := manifest.ParseDASH(fsys, "field/manifest.mpd")
	if err != nil {
		return nil, false, err
	}
	var all []model.SegmentRef
	partial := hlsStudio.Partial || hlsVoice.Partial || dashField.Partial
	for _, tl := range []*model.Timeline{hlsStudio, hlsVoice, dashField} {
		for _, role := range []model.TrackRole{model.TrackStudio, model.TrackField, model.TrackVoice} {
			all = append(all, tl.Tracks[role]...)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Track != all[j].Track {
			return all[i].Track < all[j].Track
		}
		return all[i].Index < all[j].Index
	})
	return all, partial, nil
}

func nodeID(seg model.SegmentRef) string {
	return fmt.Sprintf("leaf:%s:%d", seg.Track, seg.Index)
}

func (e *Engine) artifactPath(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(e.outDir, "artifacts", hex.EncodeToString(sum[:])+".json")
}

// segmentPath resolves a SegmentRef.URI, which the manifest parsers already
// normalize to a path relative to the fixture root, onto disk.
func (e *Engine) segmentPath(seg model.SegmentRef) string {
	return filepath.Join(e.root, filepath.FromSlash(seg.URI))
}

// fileDigest streams a sha256 over a fragment for the node input digest.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (e *Engine) writeArtifact(id string, a Artifact) error {
	data, err := json.MarshalIndent(&a, "", "  ")
	if err != nil {
		return err
	}
	final := e.artifactPath(id)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func (e *Engine) readArtifacts() ([]Artifact, error) {
	dir := filepath.Join(e.outDir, "artifacts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Artifact
	for _, ent := range entries {
		if filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			return nil, err
		}
		var a Artifact
		if err := json.Unmarshal(data, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Segment.Track != out[j].Segment.Track {
			return out[i].Segment.Track < out[j].Segment.Track
		}
		return out[i].Segment.Index < out[j].Segment.Index
	})
	return out, nil
}
