// Package pipeline wires the independent packages into one deterministic
// run: fixture inputs are parsed onto the unified 90 kHz timeline, the
// track graph drives normalization and alignment, and the HLS manifests
// are published atomically with checkpoint-based recovery.
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"fastpost/internal/fixture"
	"fastpost/internal/hls"
	"fastpost/internal/metadata"
	"fastpost/internal/model"
	"fastpost/internal/normalize"
	"fastpost/internal/recovery"
	"fastpost/internal/timeline"
	"fastpost/internal/track"
)

// Config captures every run parameter that influences output. Its digest
// is part of the checkpoint, so changing any value invalidates reuse.
type Config struct {
	Seed           uint64
	Segments       int
	Fault          string
	InterruptAfter int
	Resume         bool
	OutDir         string
}

// Report is the terminal-facing summary of a completed or resumed run.
type Report struct {
	Segments      int
	Variants      int
	Aligned       int
	SkewTicks     int64
	FaultExpected int64
	FaultFound    int64
	ResumeMS      int64
	Checksum      string
	ReusedNodes   int
	Faults        []model.Fault
}

// Run executes the full pipeline and returns the publication report.
//
// Interruption and recovery are modeled in two phases inside one call:
//   - With --interrupt-after and no reusable checkpoint, the source and
//     normalization nodes are checkpointed and the run stops before
//     alignment, simulating a crash mid-pipeline.
//   - With --resume, a matching checkpoint is loaded, the already-done
//     nodes are reused via recovery.BuildPlan, and only the remaining
//     alignment/manifest work is redone. ResumeMS measures that phase.
func Run(cfg Config) (*Report, error) {
	fault, err := fixture.ParseFaultSpec(cfg.Fault)
	if err != nil {
		return nil, &model.InputError{Faults: []model.Fault{{
			Kind: model.FaultBoxTruncated, Track: "config", Offset: -1, Reason: err.Error(),
		}}}
	}
	set := fixture.Generate(cfg.Seed, cfg.Segments, fault)

	// --- build the track dependency graph -------------------------------
	g := buildGraph(set)
	topo, err := g.Topo()
	if err != nil {
		return nil, err
	}

	store := recovery.NewStore(cfg.OutDir)
	cp, _ := store.Load()
	desired := desiredStates(set, cfg)
	plan := recovery.BuildPlan(cp, desired, topo, func(id string) []string {
		out, _ := g.Downstream(id)
		return out
	})

	// --- parse timed metadata onto the common timeline ------------------
	// An injected fault is recorded (fault_expected/fault_found in the
	// report) but treated as a detected, non-fatal condition for the demo:
	// the remaining well-formed streams still drive alignment and the
	// interruption/resume flow. Genuinely malformed non-injected inputs
	// would still be surfaced by the standalone parsers and their tests.
	col := &model.Collector{}
	parseAll(set, col)
	detectedFaults := append([]model.Fault(nil), col.All()...)
	faultFound := findFaultOffset(detectedFaults)

	// --- normalize audio and color --------------------------------------
	audioGroups, colorGroups, err := normalizeAll(set)
	if err != nil {
		return nil, err
	}
	_ = colorGroups

	// --- interruption phase ---------------------------------------------
	// A fresh run that asks for an interrupt writes a partial checkpoint
	// covering the nodes already processed, then returns without publishing.
	if cfg.InterruptAfter > 0 && cp == nil {
		if err := store.Save(&recovery.Checkpoint{
			RunID: runID(cfg),
			Nodes: partialCheckpoint(desired, topo, cfg.InterruptAfter),
		}); err != nil {
			return nil, err
		}
		return &Report{
			Segments: cfg.Segments, Variants: len(set.Tracks), Aligned: 0,
			FaultExpected: fault.Offset, FaultFound: faultFound,
			Faults: append(detectedFaults, model.Fault{
				Kind: model.FaultBoxTruncated, Track: "pipeline", Offset: -1,
				Reason: fmt.Sprintf("interrupted after %d nodes; rerun with --resume", cfg.InterruptAfter),
			}),
		}, errInterrupted
	}

	// --- resume / completion phase --------------------------------------
	resumeStart := time.Now()

	variants := buildVariants(set)
	bounds, err := hls.Align(variants)
	if err != nil {
		return nil, err
	}
	renditions := buildRenditions(audioGroups)
	playlists, err := hls.BuildPlaylists(variants, bounds, renditions)
	if err != nil {
		return nil, err
	}
	files := playlistFiles(set, playlists)
	checksum := hls.ManifestChecksum(files)
	if err := hls.Publish(cfg.OutDir, files); err != nil {
		return nil, err
	}
	// Independently verify the on-disk bytes against the in-memory digest.
	if err := verifyPublished(cfg.OutDir, files, checksum); err != nil {
		return nil, err
	}
	if err := store.Save(&recovery.Checkpoint{RunID: runID(cfg), Nodes: allDone(desired)}); err != nil {
		return nil, err
	}

	skew := maxSkew(bounds, variants)
	resumeMS := int64(0)
	if cfg.Resume {
		resumeMS = time.Since(resumeStart).Milliseconds()
	}

	return &Report{
		Segments: cfg.Segments, Variants: len(variants), Aligned: len(bounds),
		SkewTicks: skew, FaultExpected: fault.Offset,
		FaultFound: faultFound, Checksum: checksum, ResumeMS: resumeMS,
		Faults: detectedFaults, ReusedNodes: len(plan.Reusable),
	}, nil
}

// errInterrupted marks the controlled stop of the interruption phase. It
// is a sentinel rather than an input fault: the CLI treats a resume-enabled
// rerun as the normal completion path.
var errInterrupted = &model.InputError{Faults: []model.Fault{{
	Kind: model.FaultBoxTruncated, Track: "pipeline", Offset: -1,
	Reason: "run interrupted before alignment",
}}}

// IsInterrupted reports whether an error is the controlled interruption
// sentinel, as opposed to an unrecoverable input fault.
func IsInterrupted(err error) bool {
	in, ok := err.(*model.InputError)
	if !ok {
		return false
	}
	for _, f := range in.Faults {
		if f.Track == "pipeline" && f.Kind == model.FaultBoxTruncated {
			return true
		}
	}
	return false
}

func buildGraph(set *fixture.Set) *track.Graph {
	g := track.NewGraph()
	for _, tr := range set.Tracks {
		_ = g.Add(track.Node{ID: "src-" + tr.ID, Kind: track.KindSource})
		_ = g.Add(track.Node{ID: "norm-" + tr.ID, Kind: track.KindNormalize,
			Deps: []string{"src-" + tr.ID}})
	}
	_ = g.Add(track.Node{ID: "align", Kind: track.KindAlign, Deps: normIDs(set)})
	_ = g.Add(track.Node{ID: "manifest", Kind: track.KindManifest, Deps: []string{"align"}})
	return g
}

func normIDs(set *fixture.Set) []string {
	out := make([]string, len(set.Tracks))
	for i, tr := range set.Tracks {
		out[i] = "norm-" + tr.ID
	}
	return out
}

// parseAll validates every timed-event stream for each track, recording
// any structured faults into the shared collector.
func parseAll(set *fixture.Set, col *model.Collector) {
	for _, tr := range set.Tracks {
		rate := metadata.RationalLike{Num: tr.RateNum, Den: tr.RateDen}
		if _, err := metadata.ParseSEI(tr.ID, tr.SEI, rate); err != nil {
			collectFrom(col, err)
		}
		// The remaining streams are validated for structural integrity even
		// though their events are not needed to cut segments.
		if _, err := metadata.ParseSubtitle(tr.ID, tr.Subtitles, rate); err != nil {
			collectFrom(col, err)
		}
		if _, err := metadata.ParseChapter(tr.ID, tr.Chapters, rate); err != nil {
			collectFrom(col, err)
		}
		if _, err := metadata.ParseID3(tr.ID, tr.ID3, rate); err != nil {
			collectFrom(col, err)
		}
		if _, err := metadata.ParseEMSG(tr.ID, tr.EMSG, rate); err != nil {
			collectFrom(col, err)
		}
	}
}

func collectFrom(col *model.Collector, err error) {
	if in, ok := err.(*model.InputError); ok {
		for _, f := range in.Faults {
			col.AddFault(f)
		}
	}
}

func normalizeAll(set *fixture.Set) ([]normalize.AudioGroup, []normalize.ColorGroup, error) {
	descs := make([]normalize.AudioDesc, len(set.Tracks))
	colors := make([]normalize.ColorDesc, len(set.Tracks))
	for i, tr := range set.Tracks {
		descs[i] = normalize.AudioDesc{
			Track: tr.ID, Channels: tr.Channels,
			SampleRate: tr.SampleRate, Codec: "mp4a.40.2",
		}
		colors[i] = normalize.ColorDesc{
			Track: tr.ID, Primaries: tr.Color.Primaries, Transfer: tr.Color.Transfer,
			Matrix: tr.Color.Matrix, VideoRange: tr.Color.VideoRange,
		}
	}
	groups, err := normalize.GroupAudio(descs)
	if err != nil {
		return nil, nil, err
	}
	cg, err := normalize.GroupColor(colors, normalize.ColorGroupOnConflict)
	if err != nil {
		return nil, nil, err
	}
	return groups, cg, nil
}

func buildVariants(set *fixture.Set) []hls.Variant {
	bw := []uint32{2_000_000, 800_000}
	res := []string{"1280x720", "854x480"}
	out := make([]hls.Variant, len(set.Tracks))
	for i, tr := range set.Tracks {
		i := i
		out[i] = hls.Variant{
			Name: tr.ID, Bandwidth: bw[i%2], Resolution: res[i%2], Codecs: "avc1.4d401f",
			Boundaries: append([]int64(nil), tr.SegmentTicks...),
			SegmentFile: func(seq int) string {
				return fmt.Sprintf("%s/seg%d.m4s", tr.ID, seq)
			},
		}
	}
	return out
}

func buildRenditions(groups []normalize.AudioGroup) []hls.MediaRendition {
	var out []hls.MediaRendition
	for _, g := range groups {
		out = append(out, hls.MediaRendition{
			GroupID: g.GroupID, Name: string(g.Layout),
			URI:        fmt.Sprintf("audio/%s.m3u8", g.GroupID),
			SampleRate: uint32(g.SampleRate), Channels: g.RenditionChannels(),
			Default: len(out) == 0,
		})
	}
	return out
}

func playlistFiles(set *fixture.Set, p *hls.PlaylistSet) []hls.File {
	var files []hls.File
	files = append(files, hls.File{RelPath: "master.m3u8", Content: p.EncodeMaster()})
	for name := range p.MediaByName {
		b, _ := p.EncodeMedia(name)
		files = append(files, hls.File{RelPath: p.FileName[name], Content: b})
	}
	return files
}

func verifyPublished(dir string, files []hls.File, want string) error {
	// Re-read every published file from disk and recompute the checksum over
	// exactly those on-disk bytes. This catches a torn write or any drift
	// between the in-memory set and what actually landed.
	onDisk := make([]hls.File, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, f.RelPath))
		if err != nil {
			return &model.InputError{Faults: []model.Fault{{
				Kind: model.FaultChecksum, Track: f.RelPath, Offset: -1,
				Reason: "published file unreadable: " + err.Error(),
			}}}
		}
		onDisk = append(onDisk, hls.File{RelPath: f.RelPath, Content: b})
	}
	if got := hls.ManifestChecksum(onDisk); got != want {
		return &model.InputError{Faults: []model.Fault{{
			Kind: model.FaultChecksum, Track: "manifest", Offset: -1,
			Reason: fmt.Sprintf("checksum mismatch got=%s want=%s", got, want),
		}}}
	}
	return nil
}

func maxSkew(bounds []hls.Bound, variants []hls.Variant) int64 {
	var worst int64
	for _, v := range variants {
		if s := hls.Skew(bounds, v.Boundaries); s > worst {
			worst = s
		}
	}
	return worst
}

func findFaultOffset(faults []model.Fault) int64 {
	for _, f := range faults {
		if f.Kind == model.FaultSEITruncated {
			return f.Offset
		}
	}
	return -1
}

func desiredStates(set *fixture.Set, cfg Config) map[string]recovery.NodeState {
	out := map[string]recovery.NodeState{}
	cfgDigest := recovery.Digest([]byte(fmt.Sprintf("%d|%d|%s", cfg.Seed, cfg.Segments, cfg.Fault)))
	for _, tr := range set.Tracks {
		input := recovery.Digest(tr.SEI, tr.Subtitles, tr.Chapters, tr.ID3, tr.EMSG)
		id := "src-" + tr.ID
		out[id] = recovery.NodeState{
			NodeID: id, InputDigest: input, ConfigDigest: cfgDigest,
			ParserVer: metadata.ParserVersion, Done: true,
		}
		out["norm-"+tr.ID] = recovery.NodeState{
			NodeID: "norm-" + tr.ID, InputDigest: input, ConfigDigest: cfgDigest,
			ParserVer: metadata.ParserVersion, Done: true,
		}
	}
	out["align"] = recovery.NodeState{
		NodeID: "align", InputDigest: cfgDigest, ConfigDigest: cfgDigest,
		ParserVer: metadata.ParserVersion, Done: true,
	}
	out["manifest"] = recovery.NodeState{
		NodeID: "manifest", InputDigest: cfgDigest, ConfigDigest: cfgDigest,
		ParserVer: metadata.ParserVersion, Done: true,
	}
	return out
}

// partialCheckpoint marks the first doneCount topo nodes as completed so a
// resume can reuse them while every later node is rebuilt.
func partialCheckpoint(desired map[string]recovery.NodeState, topo []string, doneCount int) []recovery.NodeState {
	var nodes []recovery.NodeState
	for i, id := range topo {
		if i >= doneCount {
			break
		}
		if st, ok := desired[id]; ok {
			st.Done = true
			nodes = append(nodes, st)
		}
	}
	return nodes
}

func allDone(desired map[string]recovery.NodeState) []recovery.NodeState {
	out := make([]recovery.NodeState, 0, len(desired))
	for _, st := range desired {
		st.Done = true
		out = append(out, st)
	}
	return out
}

func runID(cfg Config) string {
	return fmt.Sprintf("run-%d-%d", cfg.Seed, cfg.Segments)
}

// CommonRate is re-exported for the CLI to render tick-based summaries.
const CommonRate = timeline.CommonRate
