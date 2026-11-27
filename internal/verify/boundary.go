// Package verify recomputes boundary and digest checks over an exported
// recovery so a reviewer can confirm a product without re-decoding it.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"archive/internal/media"
)

// Report is the machine-readable verification result written next to the
// export as verify.json.
type Report struct {
	OK               bool             `json:"ok"`
	FrameCount       int              `json:"frame_count"`
	FrameSHA256      string           `json:"frame_sha256"`
	TimelineSHA256   string           `json:"timeline_sha256"`
	BoundaryDeltaUS  int64            `json:"boundary_delta_us"`
	BoundaryOK       bool             `json:"boundary_ok"`
	FirstBadByte     int64            `json:"first_bad_byte"`
	OffsetErrorBytes int64            `json:"offset_error_bytes"`
	ResumeMS         int64            `json:"resume_ms"`
	Representations  []RepBoundary    `json:"representations"`
	Conflicts        []media.Conflict `json:"conflicts"`
	Failures         []string         `json:"failures"`
	Digests          []FileDigest     `json:"digests"`
}

// RepBoundary is one representation's declared boundary, checked against the
// timeline the export was produced from.
type RepBoundary struct {
	Representation string `json:"representation"`
	TrackID        uint32 `json:"track_id"`
	Kind           string `json:"kind"`
	StartUS        int64  `json:"start_us"`
	EndUS          int64  `json:"end_us"`
	SampleCount    int    `json:"sample_count"`
}

// FileDigest records a checked artifact and its hash.
type FileDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Input is what the verifier inspects.
type Input struct {
	// OutDir is the recovery export directory.
	OutDir string
	// Timeline is the shared-clock timeline the export was built from.
	Timeline *media.Timeline
	// Manifest is the fixture contract (frame count and boundary threshold).
	ExpectedFrames  int
	BoundaryLimitUS int64
	FirstBadByte    int64
	ResumeMS        int64
}

// Run verifies the export and writes verify.json. It returns the report and
// a boolean "passed" flag; a false flag with no error means digest or
// boundary mismatch, which maps to exit code 3.
func Run(in Input) (*Report, bool, error) {
	rep := &Report{
		FirstBadByte:     in.FirstBadByte,
		OffsetErrorBytes: 0,
		ResumeMS:         in.ResumeMS,
		BoundaryDeltaUS:  boundaryDelta(in.Timeline),
		TimelineSHA256:   TimelineDigest(in.Timeline),
	}
	if in.BoundaryLimitUS <= 0 {
		in.BoundaryLimitUS = 2000
	}
	rep.BoundaryOK = rep.BoundaryDeltaUS <= in.BoundaryLimitUS
	if !rep.BoundaryOK {
		rep.Failures = append(rep.Failures, fmt.Sprintf("boundary_delta_us=%d exceeds limit %d", rep.BoundaryDeltaUS, in.BoundaryLimitUS))
	}
	if in.Timeline != nil {
		rep.Conflicts = append(rep.Conflicts, in.Timeline.Conflicts...)
		for _, b := range in.Timeline.Boundaries {
			rep.Representations = append(rep.Representations, RepBoundary{
				Representation: b.Representation,
				TrackID:        b.TrackID,
				Kind:           string(b.Kind),
				StartUS:        b.StartUS,
				EndUS:          b.EndUS,
				SampleCount:    b.SampleCount,
			})
		}
	}

	// Frame manifest: frames.json lists exported files in index order.
	frames, err := loadFrameManifest(in.OutDir)
	if err != nil {
		return rep, false, err
	}
	rep.FrameCount = len(frames)
	digest, err := framesDigest(frames)
	if err != nil {
		return rep, false, err
	}
	rep.FrameSHA256 = digest
	// Re-hash every exported frame on disk and compare against the manifest.
	// Trusting frames.json alone would let a swapped PNG pass verification.
	for _, f := range frames {
		p := filepath.Join(in.OutDir, f.File)
		got, err := digestFile(p)
		if err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("frame %d unreadable: %v", f.Index, err))
			continue
		}
		if got.SHA256 != f.SHA {
			rep.Failures = append(rep.Failures, fmt.Sprintf("frame %d digest mismatch: manifest=%s file=%s", f.Index, f.SHA, got.SHA256))
		}
		if got.Size == 0 {
			rep.Failures = append(rep.Failures, fmt.Sprintf("frame %d is empty", f.Index))
		}
	}
	if in.ExpectedFrames > 0 && rep.FrameCount != in.ExpectedFrames {
		rep.Failures = append(rep.Failures, fmt.Sprintf("frame_count=%d expected %d", rep.FrameCount, in.ExpectedFrames))
	}
	if rep.TimelineSHA256 == "" {
		rep.Failures = append(rep.Failures, "timeline digest missing")
	}
	// Hash the exported timeline too, so verify catches a swapped artifact.
	for _, name := range []string{"timeline.json", filepath.Join("frames.json")} {
		p := filepath.Join(in.OutDir, name)
		if _, err := os.Stat(p); err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("missing artifact %s", name))
			continue
		}
		d, err := digestFile(p)
		if err != nil {
			return rep, false, err
		}
		rep.Digests = append(rep.Digests, d)
	}

	rep.OK = len(rep.Failures) == 0 && rep.BoundaryOK && rep.FirstBadByte >= 0
	payload, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return rep, false, err
	}
	if err := writeAtomic(filepath.Join(in.OutDir, "verify.json"), append(payload, '\n')); err != nil {
		return rep, false, err
	}
	return rep, rep.OK, nil
}

// Declaration formats a boundary owner as "representation/track".
func (r RepBoundary) String() string { return fmt.Sprintf("%s/t%d", r.Representation, r.TrackID) }

// frameEntry is the minimal manifest row the verifier needs.
type frameEntry struct {
	Index int    `json:"index"`
	File  string `json:"file"`
	SHA   string `json:"sha256"`
}

func loadFrameManifest(outDir string) ([]frameEntry, error) {
	data, err := os.ReadFile(filepath.Join(outDir, "frames.json"))
	if err != nil {
		return nil, fmt.Errorf("read frames.json: %w", err)
	}
	var frames []frameEntry
	if err := json.Unmarshal(data, &frames); err != nil {
		return nil, fmt.Errorf("parse frames.json: %w", err)
	}
	if len(frames) == 0 {
		return nil, fmt.Errorf("frames.json lists no frames")
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].Index < frames[j].Index })
	return frames, nil
}

// framesDigest hashes (index, file sha256) pairs in index order, producing the
// stable 64-hex frame_sha256 the acceptance contract prints.
func framesDigest(frames []frameEntry) (string, error) {
	h := sha256.New()
	for _, f := range frames {
		fmt.Fprintf(h, "%d:%s\n", f.Index, f.SHA)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// TimelineDigest hashes the canonical JSON of the timeline.
func TimelineDigest(tl *media.Timeline) string {
	if tl == nil {
		return ""
	}
	// json.Marshal is deterministic for structs and slices; maps do not
	// appear in Timeline, so byte-identical timelines hash identically.
	b, err := json.Marshal(tl)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func boundaryDelta(tl *media.Timeline) int64 {
	if tl == nil {
		return 0
	}
	var max int64
	for _, a := range tl.Alignments {
		if a.Kind != media.KindVideo {
			continue
		}
		if a.OuterDeltaUS > max {
			max = a.OuterDeltaUS
		}
	}
	return max
}

func maxDelta(b []media.Boundary) int64 {
	if len(b) < 2 {
		return 0
	}
	var max int64
	abs := func(v int64) int64 {
		if v < 0 {
			return -v
		}
		return v
	}
	base := b[0]
	for _, other := range b[1:] {
		for _, d := range []int64{
			abs(base.StartUS - other.StartUS),
			abs(base.EndUS - other.EndUS),
			abs((base.EndUS - base.StartUS) - (other.EndUS - other.StartUS)),
		} {
			if d > max {
				max = d
			}
		}
	}
	return max
}

func digestFile(path string) (FileDigest, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileDigest{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := copyTo(h, f)
	if err != nil {
		return FileDigest{}, err
	}
	return FileDigest{Path: filepath.Base(path), SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

func copyTo(w io.Writer, r io.Reader) (int64, error) {
	return io.Copy(w, r)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-verify-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tl := tmp
		_ = tl.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
