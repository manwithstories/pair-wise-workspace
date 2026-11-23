// Package pipeline wires the medpipe subsystems into one dependency-ordered
// run: read -> normalize -> index/audio/segment -> fingerprint. Every node is
// keyed by its inputs so a repaired graph resumes from the first failed node.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"medpipe/internal/audio"
	"medpipe/internal/clock"
	"medpipe/internal/dag"
	"medpipe/internal/fingerprint"
	"medpipe/internal/index"
	"medpipe/internal/model"
	"medpipe/internal/mp4sidecar"
	"medpipe/internal/segment"
	"medpipe/internal/sequence"
)

// Node names, in the order they must run.
const (
	NodeRead        = "read"
	NodeNormalize   = "normalize"
	NodeIndex       = "index"
	NodeAudio       = "audio"
	NodeSegment     = "segment"
	NodeFingerprint = "fingerprint"
)

// Config is everything a pipeline run needs.
type Config struct {
	// InputPath is the sequence file. It must live under WorkDir.
	InputPath string
	// WorkDir bounds all filesystem access.
	WorkDir string
	// StatePath is the DAG idempotency store.
	StatePath string
	// SidecarPath, when set, receives an ISO-BMFF manifest.
	SidecarPath string
	// WindowNS is the segment length.
	WindowNS int64
	// SilenceWindowNS and SilenceThreshold configure silence detection.
	SilenceWindowNS  int64
	SilenceThreshold float64
	// MaxAttempts bounds per-node retries.
	MaxAttempts int
	// RetryFailedDeps is passed to the scheduler.
	RetryFailedDeps bool
	// OnCorrupt, when set, is invoked once if the input is corrupt, allowing
	// a caller to repair the file and resume.
	OnCorrupt func(*model.Error) error
}

// Outcome is the result of a successful run.
type Outcome struct {
	Run          *dag.RunResult
	File         *sequence.File
	Timeline     *clock.Timeline
	Keyframes    *index.Index
	Silence      []audio.Range
	Segments     []segment.Segment
	Fingerprints *fingerprint.Result
	Sidecar      *mp4sidecar.Summary
	ReusedNodes  int
}

// Artifacts is shared mutable state between nodes within one process.
type Artifacts struct {
	file     *sequence.File
	timeline *clock.Timeline
	keyframe *index.Index
	silence  []audio.Range
	segs     []segment.Segment
	fps      *fingerprint.Result
	sidecar  *mp4sidecar.Summary
}

// Run executes the graph, optionally repairing a corrupt input exactly once.
func Run(cfg Config) (*Outcome, error) {
	if cfg.InputPath == "" || cfg.WorkDir == "" || cfg.StatePath == "" {
		return nil, model.New(model.KindInput, "pipeline.Run", "input, work dir and state path are required")
	}
	if cfg.WindowNS <= 0 {
		cfg.WindowNS = segment.DefaultOptions().WindowNS
	}
	if cfg.SilenceWindowNS <= 0 {
		cfg.SilenceWindowNS = audio.DefaultOptions().WindowNS
	}
	// The source digest is part of the read node's parameters. Without it a
	// stale cached read would hide a corrupted or replaced capture.
	inputDigest, err := digestFile(cfg.WorkDir, cfg.InputPath)
	if err != nil {
		return nil, err
	}
	store, err := dag.OpenStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	art := &Artifacts{}
	var corruptErr *model.Error
	repaired := false
	var outcome *Outcome
	for attempt := 0; attempt < 2; attempt++ {
		art = &Artifacts{}
		sched, err := buildGraph(cfg, store, art, &corruptErr, inputDigest)
		if err != nil {
			return nil, err
		}
		res, err := sched.Run()
		if err != nil {
			return nil, err
		}
		if len(res.FailedNodes) > 0 {
			if corruptErr != nil && cfg.OnCorrupt != nil && !repaired {
				repaired = true
				if rerr := cfg.OnCorrupt(corruptErr); rerr != nil {
					return nil, rerr
				}
				continue
			}
			if corruptErr != nil && cfg.OnCorrupt == nil {
				return nil, &Failure{Cause: corruptErr, Run: res}
			}
			for _, name := range res.FailedNodes {
				r := res.Results[name]
				return nil, &Failure{
					Cause: model.New(model.KindInput, "pipeline.Run", fmt.Sprintf("node %s failed: %s", name, r.Error)),
					Run:   res,
				}
			}
		}
		if len(res.Blocked) > 0 {
			return nil, &Failure{
				Cause: model.New(model.KindInput, "pipeline.Run", fmt.Sprintf("%d nodes did not run", len(res.Blocked))),
				Run:   res,
			}
		}
		outcome = &Outcome{
			Run: res, File: art.file, Timeline: art.timeline, Keyframes: art.keyframe,
			Silence: art.silence, Segments: art.segs, Fingerprints: art.fps,
			Sidecar: art.sidecar, ReusedNodes: res.ReusedNodes,
		}
		break
	}
	if outcome == nil {
		return nil, model.New(model.KindInput, "pipeline.Run", "pipeline did not produce a result")
	}
	return outcome, nil
}

// Failure is returned when the graph ran but one or more nodes failed. It
// carries the partial run so callers can report the first blocked dependency
// and decide what to repair.
type Failure struct {
	Cause error
	Run   *dag.RunResult
}

func (f *Failure) Error() string { return f.Cause.Error() }

func (f *Failure) Unwrap() error { return f.Cause }

// AsFailure extracts a Failure from an error chain.
func AsFailure(err error) (*Failure, bool) {
	f, ok := err.(*Failure)
	return f, ok
}

type graph struct {
	cfg   Config
	store *dag.Store
	art   *Artifacts
	// corrupt captures a typed corruption error for the repair hook.
	corrupt **model.Error
}

func buildGraph(cfg Config, store *dag.Store, art *Artifacts, corrupt **model.Error, inputDigest string) (*dag.Scheduler, error) {
	g := &graph{cfg: cfg, store: store, art: art, corrupt: corrupt}
	params := func(v any) any { return v }
	nodes := []*dag.Node{
		{Name: NodeRead, Params: params(map[string]any{
			"input": cfg.InputPath, "work": cfg.WorkDir, "sha256": inputDigest,
		}), Run: g.runRead},
		{Name: NodeNormalize, Deps: []string{NodeRead}, Params: params("v1"), Run: g.runNormalize},
		{Name: NodeIndex, Deps: []string{NodeNormalize}, Params: params("v1"), Run: g.runIndex},
		{Name: NodeAudio, Deps: []string{NodeNormalize}, Params: params(map[string]any{
			"window": cfg.SilenceWindowNS, "threshold": cfg.SilenceThreshold,
		}), Run: g.runAudio},
		// Slicing and fingerprinting both consume frame payloads (slicing to
		// validate codec framing, fingerprinting to hash content), so they
		// depend on read directly. Without that, a payload-only repair would
		// reuse results computed from different bytes.
		{Name: NodeSegment, Deps: []string{NodeIndex, NodeAudio, NodeRead}, Params: params(map[string]any{"window": cfg.WindowNS}), Run: g.runSegment},
		{Name: NodeFingerprint, Deps: []string{NodeSegment, NodeRead}, Params: params(map[string]any{"sidecar": cfg.SidecarPath != ""}), Run: g.runFingerprint},
	}
	return dag.New(nodes, store, dag.Options{MaxAttempts: cfg.MaxAttempts, RetryFailedDeps: cfg.RetryFailedDeps})
}

func (g *graph) runRead(key string) (dag.Summary, error) {
	file, err := sequence.ReadFile(g.cfg.WorkDir, g.cfg.InputPath)
	if err != nil {
		var me *model.Error
		if asModelError(err, &me) && me.Kind == model.KindCorrupt {
			*g.corrupt = me
		}
		return dag.Summary{}, err
	}
	g.art.file = file
	return dag.Summary{Digest: digestOfFrames(file), Detail: fmt.Sprintf("frames=%d", len(file.Offsets))}, nil
}

// digestFile hashes the source bytes so any modification changes the read
// node's key.
func digestFile(root, rel string) (string, error) {
	path, err := sequence.ResolveWithin(root, rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", model.Wrap(model.KindInput, "pipeline.digestFile", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (g *graph) runNormalize(key string) (dag.Summary, error) {
	if g.art.file == nil {
		return dag.Summary{}, model.New(model.KindInvariant, "pipeline.normalize", "read artifact missing")
	}
	tl, err := clock.Build(g.art.file.Header.Tracks, g.art.file.Frames, clock.Options{})
	if err != nil {
		return dag.Summary{}, err
	}
	g.art.timeline = tl
	return dag.Summary{Digest: digestOf(tl), Detail: fmt.Sprintf("tracks=%d", len(tl.Tracks))}, nil
}

func (g *graph) runIndex(key string) (dag.Summary, error) {
	if g.art.timeline == nil {
		return dag.Summary{}, model.New(model.KindInvariant, "pipeline.index", "timeline artifact missing")
	}
	idx, err := index.Build(g.art.timeline)
	if err != nil {
		return dag.Summary{}, err
	}
	g.art.keyframe = idx
	return dag.Summary{Digest: digestOfKeyframes(idx), Detail: fmt.Sprintf("keyframes=%d", len(idx.All))}, nil
}

func (g *graph) runAudio(key string) (dag.Summary, error) {
	if g.art.timeline == nil {
		return dag.Summary{}, model.New(model.KindInvariant, "pipeline.audio", "timeline artifact missing")
	}
	samples, err := audio.Collect(g.art.timeline, g.art.file.Frames)
	if err != nil {
		return dag.Summary{}, err
	}
	opt := audio.Options{WindowNS: g.cfg.SilenceWindowNS, Threshold: g.cfg.SilenceThreshold, MinSilenceNS: g.cfg.SilenceWindowNS}
	ranges, err := audio.Detect(samples, g.art.timeline.StartNS, opt)
	if err != nil {
		return dag.Summary{}, err
	}
	g.art.silence = ranges
	return dag.Summary{Digest: digestOfSilence(ranges), Detail: fmt.Sprintf("silent_ranges=%d", len(ranges))}, nil
}

func (g *graph) runSegment(key string) (dag.Summary, error) {
	if g.art.timeline == nil {
		return dag.Summary{}, model.New(model.KindInvariant, "pipeline.segment", "timeline artifact missing")
	}
	res, err := segment.Plan(g.art.timeline, g.art.silence, segment.Options{WindowNS: g.cfg.WindowNS})
	if err != nil {
		return dag.Summary{}, err
	}
	// Media framing is validated here rather than at read time: a recorder
	// can emit a frame whose bookkeeping checksum is self-consistent while
	// the payload itself is damaged. Slicing is the first consumer that must
	// trust payload content, so this is where such a frame is rejected. The
	// check runs against the planned slices so only consumed frames are read.
	if err := verifySlices(g.art.file, res.Segments); err != nil {
		var me *model.Error
		if asModelError(err, &me) && me.Kind == model.KindCorrupt {
			*g.corrupt = me
		}
		return dag.Summary{}, err
	}
	g.art.segs = res.Segments
	return dag.Summary{Digest: digestOfSegments(res.Segments), Detail: fmt.Sprintf("segments=%d", len(res.Segments))}, nil
}

func (g *graph) runFingerprint(key string) (dag.Summary, error) {
	if g.art.timeline == nil {
		return dag.Summary{}, model.New(model.KindInvariant, "pipeline.fingerprint", "timeline artifact missing")
	}
	fp, err := fingerprint.Compute(g.art.timeline, g.art.segs, g.art.file.Frames)
	if err != nil {
		return dag.Summary{}, err
	}
	g.art.fps = fp
	detail := fmt.Sprintf("segments=%d", len(fp.Segments))
	if g.cfg.SidecarPath != "" {
		if err := g.writeSidecar(); err != nil {
			return dag.Summary{}, err
		}
	}
	return dag.Summary{Digest: fp.Canonical, Detail: detail}, nil
}

// digestOfFrames is the read node's output identity: the semantic content of
// the frames, independent of file layout and metadata. Downstream keys are
// built from this digest, so nodes whose own inputs did not change are reused
// even when the capture was re-encoded.
func digestOfFrames(f *sequence.File) [32]byte {
	return sequence.DigestFrames(f.Header.Tracks, f.Frames)
}

func (g *graph) writeSidecar() error {
	path, err := sequence.ResolveWithin(g.cfg.WorkDir, g.cfg.SidecarPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return model.Wrap(model.KindInput, "pipeline.sidecar", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return model.Wrap(model.KindInput, "pipeline.sidecar", err)
	}
	defer f.Close()
	if err := mp4sidecar.Write(f, g.art.timeline, g.art.file.Frames); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return model.Wrap(model.KindInput, "pipeline.sidecar", err)
	}
	sum, err := mp4sidecar.Read(f)
	if err != nil {
		return err
	}
	g.art.sidecar = sum
	return nil
}

// digestOf hashes the timeline's per-track placement so re-timing changes the
// node key.
func digestOf(tl *clock.Timeline) [32]byte {
	h := sha256.New()
	for i := range tl.Tracks {
		tt := &tl.Tracks[i]
		h.Write([]byte(tt.Track.ID))
		h.Write([]byte{0})
		putU64(h, uint64(tt.StartNS))
		putU64(h, uint64(tt.EndNS))
		putU64(h, uint64(len(tt.Frames)))
		for _, fr := range tt.Frames {
			putU64(h, uint64(fr.NS))
			putU64(h, uint64(fr.IntervalNS))
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func digestOfKeyframes(idx *index.Index) [32]byte {
	h := sha256.New()
	for _, e := range idx.All {
		h.Write([]byte(e.TrackID))
		h.Write([]byte{0})
		putU64(h, uint64(e.NS))
		putU64(h, e.Seq)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func digestOfSilence(rs []audio.Range) [32]byte {
	h := sha256.New()
	for _, r := range rs {
		h.Write([]byte(r.TrackID))
		h.Write([]byte{0})
		putU64(h, uint64(r.StartNS))
		putU64(h, uint64(r.EndNS))
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func digestOfSegments(segs []segment.Segment) [32]byte {
	h := sha256.New()
	for _, s := range segs {
		putU64(h, uint64(s.StartNS))
		putU64(h, uint64(s.EndNS))
		for _, sl := range s.Slices {
			h.Write([]byte(sl.TrackID))
			h.Write([]byte{0})
			putU64(h, uint64(sl.FirstFrame))
			putU64(h, uint64(sl.FrameCount))
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func putU64(w io.Writer, v uint64) {
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * (7 - i)))
	}
	w.Write(b[:])
}

// ParamDigest exposes the canonical parameter encoding for tests.
func ParamDigest(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func asModelError(err error, target **model.Error) bool {
	me, ok := err.(*model.Error)
	if ok {
		*target = me
	}
	return ok
}

// verifySlices walks every frame the segments will consume and checks its
// codec framing. The first damaged frame aborts with its track, frame number
// and original byte offset.
func verifySlices(file *sequence.File, segs []segment.Segment) error {
	if file == nil {
		return model.New(model.KindInvariant, "pipeline.verifySlices", "no file artifact")
	}
	kinds := make(map[string]model.Kind, len(file.Header.Tracks))
	for _, tr := range file.Header.Tracks {
		kinds[tr.ID] = tr.Kind
	}
	seen := map[string]map[int]bool{}
	for _, seg := range segs {
		for _, sl := range seg.Slices {
			fs := file.Frames[sl.TrackID]
			if seen[sl.TrackID] == nil {
				seen[sl.TrackID] = map[int]bool{}
			}
			for k := sl.FirstFrame; k < sl.FirstFrame+sl.FrameCount; k++ {
				if k < 0 || k >= len(fs) {
					return model.New(model.KindInvariant, "pipeline.verifySlices",
						fmt.Sprintf("track %s frame index %d out of range", sl.TrackID, k))
				}
				if seen[sl.TrackID][k] {
					continue
				}
				seen[sl.TrackID][k] = true
				if err := model.ValidatePayload(kinds[sl.TrackID], &fs[k]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
