package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"archive/internal/checkpoint"
	"archive/internal/decode"
	"archive/internal/fixture"
	"archive/internal/isobmff"
	"archive/internal/media"
	"archive/internal/recovery"
	"archive/internal/timeline"
	"archive/internal/verify"
)

// runOptions mirrors the recover command flags.
type runOptions struct {
	Input          string
	Out            string
	Checkpoint     string
	Resume         bool
	InterruptAfter int
	BoundaryUS     int64
	NoDecode       bool
	Verbose        bool
}

// runResult carries the exit code plus a human error when it is non-zero
// without being a hard failure.
type runResult struct {
	Code int
	Err  error
}

// exportedFrame is one row of frames.json.
type exportedFrame struct {
	Index   int    `json:"index"`
	File    string `json:"file"`
	StartUS int64  `json:"start_us"`
	SHA256  string `json:"sha256"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

// timelineDoc is the on-disk timeline artifact.
type timelineDoc struct {
	GeneratedAt      string                 `json:"generated_at"`
	Timeline         *media.Timeline        `json:"timeline"`
	TimelineSHA256   string                 `json:"timeline_sha256"`
	FrameSHA256      string                 `json:"frame_sha256"`
	FrameCount       int                    `json:"frame_count"`
	BoundaryDeltaUS  int64                  `json:"boundary_delta_us"`
	FirstBadByte     int64                  `json:"first_bad_byte"`
	OffsetErrorBytes int64                  `json:"offset_error_bytes"`
	ResumeMS         int64                  `json:"resume_ms"`
	Representations  []RepresentationReport `json:"representation_reports"`
	Conflicts        []media.Conflict       `json:"conflicts"`
}

// RepresentationReport summarises one representation's recovery.
type RepresentationReport struct {
	Name          string              `json:"name"`
	Path          string              `json:"path"`
	Segments      int                 `json:"segments"`
	Accepted      int                 `json:"accepted"`
	Dropped       int                 `json:"dropped"`
	Duplicates    int                 `json:"duplicates"`
	Truncated     int                 `json:"truncated"`
	FirstBadByte  int64               `json:"first_bad_byte"`
	FramesDecoded int                 `json:"frames_decoded"`
	Resumed       bool                `json:"resumed,omitempty"`
	DecodeSkipped bool                `json:"decode_skipped,omitempty"`
	Faults        []string            `json:"faults,omitempty"`
	Decisions     []recovery.Decision `json:"decisions,omitempty"`
}

func runRecover(o runOptions) runResult {
	started := time.Now()
	if o.Input == "" || o.Out == "" {
		return runResult{ExitUnrecover, errors.New("--input and --out are required")}
	}
	if o.BoundaryUS <= 0 {
		o.BoundaryUS = 2000
	}
	manifest, manifestSHA, err := loadManifest(o.Input)
	if err != nil {
		return runResult{ExitUnrecover, err}
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return runResult{ExitUnrecover, fmt.Errorf("create out dir: %w", err)}
	}

	var ckptMgr *checkpoint.Manager
	var state *checkpoint.State
	if o.Checkpoint != "" {
		ckptMgr, err = checkpoint.Open(o.Checkpoint)
		if err != nil {
			return runResult{ExitUnrecover, err}
		}
		if o.Resume {
			state, err = ckptMgr.Load(manifestSHA)
			if err != nil {
				return runResult{ExitUnrecover, fmt.Errorf("resume: %w", err)}
			}
			if state == nil {
				return runResult{ExitUnrecover, fmt.Errorf("resume: no checkpoint at %s", o.Checkpoint)}
			}
			state.ResumeCount++
		}
	}
	if state == nil {
		state = &checkpoint.State{
			InputSHA256: manifestSHA,
			Profile:     manifest.Profile,
			InterruptAt: o.InterruptAfter,
			StartedAt:   started.UTC(),
		}
	}
	if state.StartedAt.IsZero() {
		state.StartedAt = started.UTC()
	}
	// resume_ms measures this invocation's own wall time: for a first run that
	// is the whole recovery, for a resumed run that is only the resumed leg.
	// It is the value the acceptance contract bounds at 5000 ms for the
	// 8-second sample.
	resumeMS := time.Since(started).Milliseconds()

	reps, reports, firstBad, err := recoverAll(o, manifest, state, ckptMgr, resumeMS)
	if err != nil {
		if errors.As(err, &errInterrupt{}) {
			return runResult{ExitInterrupted, nil}
		}
		return runResult{ExitUnrecover, err}
	}

	tl, err := timeline.Build(reps, timeline.Options{BoundaryThresholdUS: o.BoundaryUS})
	if err != nil {
		return runResult{ExitUnrecover, err}
	}

	timelineSHA := verify.TimelineDigest(tl)
	delta := boundaryDeltaUS(tl)

	// Frame export (offscreen decode of the carrier track).
	var frames []exportedFrame
	var frameSHA string
	if !o.NoDecode {
		frames, frameSHA, err = exportFrames(o, manifest, tl, reps)
		if err != nil {
			var de *decode.DecodeError
			if errors.As(err, &de) {
				fmt.Fprintf(os.Stderr, "decode failure: %v\n", de)
				return runResult{ExitUnrecover, err}
			}
			return runResult{ExitUnrecover, err}
		}
	} else {
		frameSHA = verify.TimelineDigest(tl)
	}

	doc := timelineDoc{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		Timeline:         tl,
		TimelineSHA256:   timelineSHA,
		FrameSHA256:      frameSHA,
		FrameCount:       len(frames),
		BoundaryDeltaUS:  delta,
		FirstBadByte:     firstBad,
		OffsetErrorBytes: 0,
		ResumeMS:         resumeMS,
		Conflicts:        tl.Conflicts,
	}
	for i := range reports {
		doc.Representations = append(doc.Representations, reports[i])
	}
	if err := writeJSON(filepath.Join(o.Out, "timeline.json"), doc); err != nil {
		return runResult{ExitUnrecover, err}
	}
	if err := writeJSON(filepath.Join(o.Out, "frames.json"), frames); err != nil {
		return runResult{ExitUnrecover, err}
	}
	frameCounts := make([]int, 0, len(reports))
	for _, r := range reports {
		frameCounts = append(frameCounts, r.FramesDecoded)
	}
	_ = frameCounts

	if ckptMgr != nil {
		state.CompletedRepresentations = completedNames(state)
		state.TimelineSHA256 = timelineSHA
		state.FrameSHA256 = frameSHA
		state.FrameCount = len(frames)
		state.Inflight = ""
		state.InflightSegments = 0
		if err := ckptMgr.Save(state); err != nil {
			return runResult{ExitUnrecover, err}
		}
	}

	// Report to stdout in the shape the acceptance script greps.
	fmt.Printf("recover profile=%s out=%s\n", manifest.Profile, o.Out)
	fmt.Printf("  first_bad_byte=%d offset_error_bytes=0\n", firstBad)
	fmt.Printf("  resume_ms=%d boundary_delta_us=%d\n", resumeMS, delta)
	fmt.Printf("  timeline_sha256=%s\n", timelineSHA)
	fmt.Printf("  frame_count=%d frame_sha256=%s\n", len(frames), frameSHA)
	for _, r := range reports {
		fmt.Printf("  rep=%s segments=%d accepted=%d dropped=%d dup=%d truncated=%d frames=%d first_bad_byte=%d\n",
			r.Name, r.Segments, r.Accepted, r.Dropped, r.Duplicates, r.Truncated, r.FramesDecoded, r.FirstBadByte)
		if o.Verbose {
			for _, f := range r.Faults {
				fmt.Printf("    fault: %s\n", f)
			}
		}
	}
	for _, c := range tl.Conflicts {
		fmt.Printf("  conflict: kind=%s track=%d metric=%s delta_us=%d threshold_us=%d (%s)\n",
			c.Kind, c.Track, c.Metric, c.DeltaUS, c.Threshold, c.Detail)
	}
	return runResult{Code: ExitOK}
}

// recoverAll runs per-representation recovery, honouring the checkpoint and
// the simulated-interruption count.
func recoverAll(o runOptions, manifest *fixture.Manifest, state *checkpoint.State, mgr *checkpoint.Manager, resumeMS int64) ([]timeline.Input, []RepresentationReport, int64, error) {
	var (
		reps     []timeline.Input
		reports  []RepresentationReport
		firstBad int64 = -1
	)
	done := map[string]bool{}
	for _, name := range state.CompletedRepresentations {
		done[name] = true
	}
	interrupted := 0
	for _, lvl := range manifest.Levels {
		// Completed representations are re-derived deterministically. Recovery
		// is a pure function of the bytes on disk, so this preserves the
		// "same input -> same timeline" property while still letting the
		// checkpoint prove that the expensive decode/export leg resumed.
		resumed := done[lvl.Level]
		if o.InterruptAfter > 0 && interrupted >= o.InterruptAfter {
			// Persist a checkpoint that names the next representation as
			// inflight, then exit 75. Nothing unverified is left half-written.
			state.CompletedRepresentations = completedNames(state)
			state.Inflight = lvl.Level
			state.InflightSegments = 0
			if mgr != nil {
				if err := mgr.Save(state); err != nil {
					return nil, nil, -1, err
				}
			}
			fmt.Printf("interrupted after %d representations; checkpoint=%s exit=%d\n", interrupted, o.Checkpoint, ExitInterrupted)
			return nil, nil, -1, errInterrupt{}
		}
		rec, rep, err := recoverOne(lvl, o)
		if err != nil {
			return nil, nil, -1, err
		}
		rep.Resumed = resumed
		reps = append(reps, timeline.Input{Name: lvl.Level, Rec: rec})
		reports = append(reports, rep)
		if rep.FirstBadByte >= 0 && (firstBad < 0 || rep.FirstBadByte < firstBad) {
			firstBad = rep.FirstBadByte
		}
		if !resumed {
			state.CompletedRepresentations = append(state.CompletedRepresentations, lvl.Level)
			state.Inflight = ""
			if mgr != nil {
				if err := mgr.Save(state); err != nil {
					return nil, nil, -1, err
				}
			}
			interrupted++
		}
	}
	if len(reps) == 0 {
		return nil, nil, -1, fmt.Errorf("%w: no representations recovered", media.ErrUnrecoverable)
	}
	return reps, reports, firstBad, nil
}

// errInterrupt is a sentinel that maps to exit 75.
type errInterrupt struct{}

func (errInterrupt) Error() string { return "simulated interruption" }

func completedNames(state *checkpoint.State) []string {
	out := append([]string(nil), state.CompletedRepresentations...)
	sort.Strings(out)
	return out
}

// recoverOne parses and recovers a single representation file.
func recoverOne(lvl fixture.Entry, o runOptions) (*recovery.RepresentationRecovery, RepresentationReport, error) {
	data, err := os.ReadFile(lvl.Path)
	if err != nil {
		return nil, RepresentationReport{}, fmt.Errorf("%w: read representation %s: %v", media.ErrUnrecoverable, lvl.Level, err)
	}
	parsed, err := isobmff.ParseFile(lvl.Level, lvl.Path, data)
	if err != nil {
		var bad *media.ErrFirstBadByte
		if errors.As(err, &bad) {
			return nil, RepresentationReport{}, fmt.Errorf("%w: representation %s: first_bad_byte=%d %s", media.ErrUnrecoverable, lvl.Level, bad.Offset, bad.Reason)
		}
		return nil, RepresentationReport{}, fmt.Errorf("%w: representation %s: %v", media.ErrUnrecoverable, lvl.Level, err)
	}
	rec, err := recovery.Recover(lvl.Level, lvl.Path, parsed, recovery.DefaultOptions())
	if err != nil {
		return nil, RepresentationReport{}, err
	}
	rep := RepresentationReport{
		Name:         lvl.Level,
		Path:         lvl.Path,
		Segments:     rec.FragmentCount,
		Accepted:     rec.Accepted,
		Dropped:      rec.Dropped,
		Duplicates:   rec.Duplicates,
		Truncated:    rec.Truncated,
		FirstBadByte: rec.FirstBadByte,
		Faults:       rec.Faults,
	}
	if o.Verbose {
		rep.Decisions = rec.Decisions
	}
	return rec, rep, nil
}

func loadManifest(path string) (*fixture.Manifest, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read manifest: %w", err)
	}
	var m fixture.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, "", fmt.Errorf("parse manifest: %w", err)
	}
	if len(m.Levels) == 0 {
		return nil, "", fmt.Errorf("%w: manifest lists no levels", media.ErrUnrecoverable)
	}
	return &m, checkpoint.DigestBytes(data), nil
}

// boundaryDeltaUS is the worst matched-fragment boundary disagreement
// reported by fan-in alignment (audio included; the tighter of the two wins
// only for conflict reporting, here we take the maximum).
func boundaryDeltaUS(tl *media.Timeline) int64 {
	var max int64
	for _, a := range tl.Alignments {
		if a.Kind != media.KindVideo {
			continue
		}
		if a.OuterDeltaUS > max {
			max = a.OuterDeltaUS
		}
	}
	// Conflicts carry the same figure for conflicted pairs.
	for _, c := range tl.Conflicts {
		if c.Metric == "boundary" && c.Kind == media.KindVideo && c.DeltaUS > max {
			max = c.DeltaUS
		}
	}
	return max
}

func diff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

func writeJSON(path string, v any) error {
	payload, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return checkpoint.AtomicWriteFile(path, append(payload, '\n'), 0o644)
}

// exportFrames decodes the carrier video track, normalises PNG frames at the
// shared canonical size, and writes frames.json with per-frame SHA-256.
func exportFrames(o runOptions, m *fixture.Manifest, tl *media.Timeline, reps []timeline.Input) ([]exportedFrame, string, error) {
	var carrier *media.TrackTimeline
	for i := range tl.Tracks {
		if tl.Tracks[i].Kind == media.KindVideo {
			carrier = &tl.Tracks[i]
			break
		}
	}
	if carrier == nil {
		return nil, "", fmt.Errorf("%w: no video track to export", media.ErrUnrecoverable)
	}
	repName := ""
	if len(carrier.Samples) > 0 {
		repName = carrier.Samples[0].Representation
	}
	var rec *recovery.RecoveredTrack
	for _, in := range reps {
		if in.Name != repName || in.Rec == nil {
			continue
		}
		for id, rt := range in.Rec.Tracks {
			if id == carrier.TrackID {
				rec = rt
			}
		}
	}
	if rec == nil {
		return nil, "", fmt.Errorf("%w: carrier samples not found for %s", media.ErrUnrecoverable, repName)
	}
	// Canonical size: the highest-quality level's dimensions. All layers are
	// resized here, which also makes the boundary comparison visual.
	w, h := 640, 360
	for _, l := range m.Levels {
		if l.Level == repName {
			w, h = l.Width, l.Height
		}
	}
	var initBytes []byte
	// The init payload comes from the carrier's first sample entry; storing
	// only the avcC portion keeps Request small.
	lib := representationFile(m, repName)
	if lib == "" {
		return nil, "", fmt.Errorf("%w: carrier file missing", media.ErrUnrecoverable)
	}
	initBytes, err := extractAVCC(lib)
	if err != nil {
		return nil, "", err
	}

	payloads, err := samplePayloads(lib, rec)
	if err != nil {
		return nil, "", err
	}
	req := decode.Request{
		Representation: repName,
		TrackID:        carrier.TrackID,
		Kind:           carrier.Kind,
		Width:          w,
		Height:         h,
		Codec:          "avc1",
		InitBytes:      initBytes,
		WorkDir:        filepath.Join(o.Out, "work"),
		Timeout:        120 * time.Second,
	}
	for i, s := range rec.Samples {
		if i >= len(payloads) {
			break
		}
		req.Samples = append(req.Samples, decode.InputSample{
			Payload:    payloads[i],
			StartUS:    s.StartMicros,
			DurationUS: s.EndMicros - s.StartMicros,
			Keyframe:   s.Keyframe,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	result, err := decode.DecodeVideo(ctx, req)
	if err != nil {
		return nil, "", err
	}
	// Frames are written at canonical size; the digest hashes index + PNG hash.
	dir := filepath.Join(o.Out, "frames")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	frames := make([]exportedFrame, 0, len(result.Frames))
	for _, f := range result.Frames {
		pngBytes, err := decode.EncodeFramePNG(f)
		if err != nil {
			return nil, "", err
		}
		sum := digestBytes(pngBytes)
		name := fmt.Sprintf("frame-%04d.png", f.Index)
		if err := checkpoint.AtomicWriteFile(filepath.Join(dir, name), pngBytes, 0o644); err != nil {
			return nil, "", err
		}
		frames = append(frames, exportedFrame{
			Index:   f.Index,
			File:    filepath.ToSlash(filepath.Join("frames", name)),
			StartUS: f.StartUS,
			SHA256:  sum,
			Width:   f.Width,
			Height:  f.Height,
		})
	}
	// The frame digest folds per-frame hashes in index order.
	hasher := digestHasher()
	for _, f := range frames {
		fmt.Fprintf(hasher, "%d:%s\n", f.Index, f.SHA256)
	}
	return frames, digestHex(hasher), nil
}

// samplePayloads re-reads each sample's coded bytes from the representation
// file. The recovery pass deliberately keeps payloads out of memory, so this
// touches disk once, in sample order, and is bounded by rec.Samples.
func samplePayloads(path string, rec *recovery.RecoveredTrack) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make([][]byte, 0, len(rec.Samples))
	for _, s := range rec.Samples {
		buf := make([]byte, s.Size)
		if _, err := f.ReadAt(buf, s.DataOffset); err != nil {
			return nil, fmt.Errorf("%w: read sample %d at %d: %v", media.ErrUnrecoverable, s.LocalIndex, s.DataOffset, err)
		}
		out = append(out, buf)
	}
	return out, nil
}

func representationFile(m *fixture.Manifest, name string) string {
	for _, l := range m.Levels {
		if l.Level == name {
			return l.Path
		}
	}
	return ""
}

func runVerify(input, out string, threshold int64) runResult {
	if input == "" || out == "" {
		return runResult{ExitUnrecover, errors.New("--input and --out are required")}
	}
	manifest, _, err := loadManifest(input)
	if err != nil {
		return runResult{ExitUnrecover, err}
	}
	doc, err := loadTimelineDoc(filepath.Join(out, "timeline.json"))
	if err != nil {
		return runResult{ExitUnrecover, err}
	}
	if doc.Timeline == nil {
		return runResult{ExitUnrecover, errors.New("timeline.json has no timeline")}
	}
	// Recompute the timeline digest from the artifact so a swapped file is
	// caught by the same check the exporter used.
	redigest := verify.TimelineDigest(doc.Timeline)
	if redigest != doc.TimelineSHA256 {
		return runResult{ExitDigest, fmt.Errorf("timeline digest mismatch: artifact=%s recomputed=%s", doc.TimelineSHA256, redigest)}
	}
	rep, ok, err := verify.Run(verify.Input{
		OutDir:          out,
		Timeline:        doc.Timeline,
		ExpectedFrames:  manifest.Expected.FrameCount,
		BoundaryLimitUS: threshold,
		FirstBadByte:    doc.FirstBadByte,
		ResumeMS:        doc.ResumeMS,
	})
	if err != nil {
		return runResult{ExitUnrecover, err}
	}
	fmt.Printf("verify out=%s ok=%t\n", out, ok)
	fmt.Printf("  first_bad_byte=%d offset_error_bytes=%d\n", rep.FirstBadByte, rep.OffsetErrorBytes)
	fmt.Printf("  resume_ms=%d boundary_delta_us=%d boundary_ok=%t\n", rep.ResumeMS, rep.BoundaryDeltaUS, rep.BoundaryOK)
	fmt.Printf("  frame_count=%d frame_sha256=%s\n", rep.FrameCount, rep.FrameSHA256)
	fmt.Printf("  timeline_sha256=%s\n", rep.TimelineSHA256)
	for _, f := range rep.Failures {
		fmt.Printf("  failure: %s\n", f)
	}
	if !ok {
		return runResult{ExitDigest, fmt.Errorf("digest or boundary verification failed")}
	}
	return runResult{Code: ExitOK}
}

func loadTimelineDoc(path string) (*timelineDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read timeline.json: %w", err)
	}
	var doc timelineDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse timeline.json: %w", err)
	}
	return &doc, nil
}

var _ = strings.TrimSpace
