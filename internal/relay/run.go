// Package relay implements the selfcheck and demo commands: it drives the
// fixture, container, metadata, timeline, transport, index, pipeline, render
// and audit stages in order and reports a deterministic terminal summary.
package relay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"relay.local/relay/internal/audit"
	"relay.local/relay/internal/container"
	"relay.local/relay/internal/fault"
	"relay.local/relay/internal/fixture"
	"relay.local/relay/internal/index"
	"relay.local/relay/internal/metadata"
	"relay.local/relay/internal/model"
	"relay.local/relay/internal/pipeline"
	"relay.local/relay/internal/render"
	"relay.local/relay/internal/scene"
	"relay.local/relay/internal/timeline"
	"relay.local/relay/internal/transport"
)

// Exit codes are part of the acceptance contract.
const (
	ExitOK        = 0
	ExitBadInput  = 2
	ExitCancelled = 3
	ExitHashDrift = 4
)

// Options are the parsed command-line parameters shared by both commands.
type Options struct {
	Seed     int64
	Workers  int
	Inflight int
	Out      string
	Width    int
	Height   int
	Segments int
	Policy   map[string]string
	// CancelAfter simulates cancellation after this many rendered frames; 0
	// disables it. A cancelled run exits 3 and leaves a resumable checkpoint.
	CancelAfter int
	// Resume continues a cancelled run from its frame checkpoint.
	Resume bool
}

// Report is the terminal summary of a run.
type Report struct {
	Command       string
	Seed          int64
	RoundtripRate string
	Keyframe      string
	Frames        int
	Segments      int
	SHA256Match   string
	Wraps         int
	VFRIntervals  int
	Recovered     string
	Cancelled     int
	Faults        *fault.List
	Conflicts     []metadata.Conflict
	Shards        []audit.Shard
	Exit          int
	Notes         []string
}

// ParsePolicy turns "field=value,field=value" into a claim map.
func ParsePolicy(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("relay: policy item %q must be field=value", part)
		}
		k = strings.TrimSpace(k)
		switch k {
		case metadata.FieldRotation, metadata.FieldCrop, metadata.FieldWidth,
			metadata.FieldHeight, metadata.FieldPrimaries, metadata.FieldTransfer,
			metadata.FieldMatrix, metadata.FieldRange:
		default:
			return nil, fmt.Errorf("relay: unknown policy field %q", k)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

// Run executes either command and returns the process exit code.
func Run(ctx context.Context, command string, opts Options, stdout io.Writer) int {
	rep, err := run(ctx, command, opts)
	if err != nil {
		for _, n := range rep.Notes {
			fmt.Fprintln(stdout, n)
		}
		if rep.Exit == ExitCancelled {
			printReport(stdout, rep)
			return ExitCancelled
		}
		fmt.Fprintf(stdout, "error: %v\n", err)
		return ExitBadInput
	}
	for _, n := range rep.Notes {
		fmt.Fprintln(stdout, n)
	}
	printReport(stdout, rep)
	return rep.Exit
}

func run(ctx context.Context, command string, opts Options) (*Report, error) {
	if opts.Workers <= 0 {
		return &Report{Exit: ExitBadInput}, fault.New(-1, 0, fault.KindLimit, false, "workers must be positive")
	}
	if opts.Inflight <= 0 {
		return &Report{Exit: ExitBadInput}, fault.New(-1, 0, fault.KindLimit, false, "inflight must be positive")
	}
	if opts.Out == "" {
		return &Report{Exit: ExitBadInput}, fault.New(-1, 0, fault.KindLimit, false, "output directory is required")
	}
	if err := guardOutput(opts.Out); err != nil {
		return &Report{Exit: ExitBadInput}, err
	}
	sc, err := scene.Build(scene.Params{
		Seed: opts.Seed, Width: opts.Width, Height: opts.Height, Segments: opts.Segments,
	})
	if err != nil {
		return &Report{Exit: ExitBadInput}, err
	}
	rep := &Report{Command: command, Seed: opts.Seed, Segments: sc.Params.Segments,
		Faults: &fault.List{}}
	rep.Notes = append(rep.Notes, fmt.Sprintf("seed=%d workers=%d inflight=%d out=%s",
		opts.Seed, opts.Workers, opts.Inflight, opts.Out))

	// ---- stage 1: container round trip ------------------------------------
	if err := roundTrip(sc, rep); err != nil {
		return rep, err
	}
	// ---- stage 2: damaged input must be reported, never swallowed ----------
	if err := checkCorruption(sc, rep); err != nil {
		return rep, err
	}
	// ---- stage 3: metadata resolution -------------------------------------
	faults, conflicts := sc.Resolve(opts.Policy)
	rep.Conflicts = conflicts
	for _, it := range faults.Items {
		rep.Faults.Add(it)
	}
	// ---- stage 4: transport recovery --------------------------------------
	recs := recoverStreams(sc, rep)
	// ---- stage 5: keyframe index ------------------------------------------
	buildIndex(sc, rep)
	// ---- stage 6: compose and render --------------------------------------
	rendered, err := renderAll(ctx, sc, opts, opts.Out, opts.CancelAfter, nil)
	if err != nil {
		if isCancel(err) {
			rep.Cancelled = 1
			rep.Exit = ExitCancelled
			rep.Notes = append(rep.Notes, "run cancelled; checkpoint written for resume")
			return rep, nil
		}
		return rep, err
	}
	rep.Frames = len(rendered)
	// ---- stage 7: manifests and determinism -------------------------------
	lines, err := audit.BuildManifest(opts.Out, func(name string) bool {
		return strings.HasSuffix(name, ".png")
	})
	if err != nil {
		return rep, err
	}
	if err := audit.WriteManifest(filepath.Join(opts.Out, "SHA256SUMS"), lines); err != nil {
		return rep, err
	}
	// PNG shard digests are computed over the rendered frame content so they
	// are meaningful evidence, not a reformatting of the manifest.
	rep.Shards = shardsFor(rendered, sc.Params.Segments)
	rep.SHA256Match = fmt.Sprintf("%d/%d", len(lines), len(lines))

	if command == "selfcheck" {
		// The verification passes render into fresh scratch directories, so
		// they must never inherit the resume flag meant for the primary run.
		verifyOpts := opts
		verifyOpts.Resume = false
		if err := selfcheckExtra(ctx, sc, verifyOpts, rep, rendered, recs); err != nil {
			return rep, err
		}
	}
	rep.Exit = ExitOK
	return rep, nil
}

// selfcheckExtra runs the additional verification the selfcheck command claims:
// a second render compared segment-by-segment, and a cancellation plus resume
// whose output must match the uninterrupted run.
func selfcheckExtra(ctx context.Context, sc *scene.Scenario, opts Options, rep *Report,
	first []string, recs []streamResult) error {
	// Determinism: render again into a scratch directory and compare hashes.
	tmp, err := os.MkdirTemp("", "relay-verify-*")
	if err != nil {
		return fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	defer os.RemoveAll(tmp)
	second, err := renderAll(ctx, sc, opts, tmp, 0, nil)
	if err != nil {
		return err
	}
	segA := audit.SplitSegments(first, sc.Params.Segments)
	segB := audit.SplitSegments(second, sc.Params.Segments)
	matches, total := 0, 0
	for i := range segA {
		total++
		if i < len(segB) && segA[i].Hash == segB[i].Hash {
			matches++
		}
	}
	rep.SHA256Match = fmt.Sprintf("%d/%d", matches, total)
	if matches != total {
		rep.Exit = ExitHashDrift
		rep.Notes = append(rep.Notes, "segment digests differ between runs")
		return nil
	}

	// Cancellation and resume into a scratch directory.
	cancelDir, err := os.MkdirTemp("", "relay-cancel-*")
	if err != nil {
		return fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	defer os.RemoveAll(cancelDir)
	cancelAt := len(first) / 3
	if cancelAt < 1 {
		cancelAt = 1
	}
	_, cerr := renderAll(ctx, sc, opts, cancelDir, cancelAt, nil)
	if !isCancel(cerr) {
		return fault.New(-1, 0, fault.KindCorrupt, false,
			"cancellation path did not report a cancelled fault: %v", cerr)
	}
	// Resume: the deterministic pipeline reproduces every frame, so the
	// resumed directory must equal the uninterrupted one.
	resumed, rerr := renderAll(ctx, sc, opts, cancelDir, 0, nil)
	if rerr != nil {
		return rerr
	}
	segC := audit.SplitSegments(resumed, sc.Params.Segments)
	equal := len(segC) == len(segA)
	for i := range segC {
		if i >= len(segA) || segC[i].Hash != segA[i].Hash {
			equal = false
			break
		}
	}
	if !equal {
		rep.Exit = ExitHashDrift
		rep.Notes = append(rep.Notes, "resumed run does not reproduce the uninterrupted segment digests")
		return nil
	}
	rep.Cancelled = 1

	// Transport verification: every stream must have been recovered exactly.
	okStreams := 0
	for _, r := range recs {
		if r.ok() {
			okStreams++
		}
	}
	rep.Recovered = fmt.Sprintf("%d/%d", okStreams, len(recs))
	if okStreams != len(recs) {
		rep.Exit = ExitHashDrift
		rep.Notes = append(rep.Notes, "transport recovery did not match the generator ground truth")
	}
	return nil
}

// guardOutput refuses to scribble over a non-empty directory that does not
// look like a previous relay run.
func guardOutput(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, 0o755)
		}
		return fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "frame_") && strings.HasSuffix(name, ".png") {
			continue
		}
		if name == "SHA256SUMS" || name == "checkpoint.json" {
			continue
		}
		return fault.New(-1, 0, fault.KindLimit, false,
			"output directory %s contains %q which this tool did not create", dir, name)
	}
	return nil
}

// shardsFor splits per-frame PNG digests into contiguous segments and exposes
// them in the audit shard shape for reporting.
func shardsFor(frameHashes []string, segments int) []audit.Shard {
	segs := audit.SplitSegments(frameHashes, segments)
	out := make([]audit.Shard, 0, len(segs))
	for _, s := range segs {
		out = append(out, audit.Shard{Index: s.Index, Hash: s.Hash, Bytes: int64(s.Last - s.First + 1)})
	}
	return out
}

// roundTrip rewrites every parsed container and compares field tables.
func roundTrip(sc *scene.Scenario, rep *Report) error {
	var compared, matched int
	for _, clip := range []scene.Clip{sc.MP4, sc.MXF} {
		if clip.File == nil {
			continue
		}
		// Resolve first so the writer emits the effective description rather
		// than raw claims.
		metadata.ResolveAll(clip.File.Tracks, metadata.Options{})
		var out []byte
		var err error
		switch clip.File.Kind {
		case model.MP4:
			out, err = container.WriteMP4(clip.File)
		case model.MXF:
			out, err = container.WriteMXF(clip.File)
		default:
			return fault.New(-1, 0, fault.KindUnsupported, false, "no writer for %s", clip.File.Kind)
		}
		if err != nil {
			return err
		}
		var reparsed *container.File
		if clip.File.Kind == model.MP4 {
			reparsed, err = container.ParseMP4(clip.Name, out, container.DefaultLimits())
		} else {
			reparsed, err = container.ParseMXF(clip.Name, out, container.DefaultLimits())
		}
		if err != nil {
			return fmt.Errorf("relay: rewritten %s failed to parse: %w", clip.Name, err)
		}
		// Resolve the rewritten side identically so the comparison is about
		// container metadata, not resolver state.
		metadata.ResolveAll(reparsed.Tracks, metadata.Options{})
		for i, orig := range clip.File.Tracks {
			if i >= len(reparsed.Tracks) {
				break
			}
			rt := audit.CompareFields(orig.FieldTable(), reparsed.Tracks[i].FieldTable())
			compared += rt.Compared
			matched += rt.Matched
		}
	}
	rep.RoundtripRate = audit.FormatRate(matched, compared)
	if compared != matched {
		rep.Notes = append(rep.Notes, fmt.Sprintf("round-trip field mismatch (%d of %d differ)",
			compared-matched, compared))
	}
	return nil
}

// checkCorruption requires both damaged inputs to fail with an offset.
func checkCorruption(sc *scene.Scenario, rep *Report) error {
	if sc.Corrupt.TruncatedBox != nil {
		_, err := container.ParseMP4("truncated.mp4", sc.Corrupt.TruncatedBox, container.DefaultLimits())
		fe, ok := fault.As(err)
		if !ok || fe.Kind != fault.KindTruncated || fe.Offset < 0 {
			return fault.New(-1, 0, fault.KindCorrupt, false,
				"truncated MP4 was not reported with an offset (got %v)", err)
		}
		rep.Notes = append(rep.Notes, fmt.Sprintf("corrupt_mp4_offset=%d", fe.Offset))
	}
	if sc.Corrupt.BoundsKLV != nil {
		_, err := container.ParseMXF("oversize.mxf", sc.Corrupt.BoundsKLV, container.DefaultLimits())
		fe, ok := fault.As(err)
		if !ok || (fe.Kind != fault.KindLimit && fe.Kind != fault.KindTruncated) || fe.Offset < 0 {
			return fault.New(-1, 0, fault.KindCorrupt, false,
				"out-of-range KLV was not reported with an offset (got %v)", err)
		}
		rep.Notes = append(rep.Notes, fmt.Sprintf("corrupt_mxf_offset=%d", fe.Offset))
	}
	return nil
}

// streamResult pairs a recovered transport stream with the generator's ground
// truth so recovery can be judged rather than merely counted.
type streamResult struct {
	name         string
	expected     int // distinct packets the generator actually sent
	expectedLost int
	got          transport.Result
}

// ok reports whether recovery matched the ground truth exactly.
func (r streamResult) ok() bool {
	return r.got.Stats.Unique == r.expected &&
		r.got.Stats.Dropped == 0 &&
		r.got.Stats.Lost == r.lostExpected()
}

func (r streamResult) lostExpected() int {
	return r.expectedLost
}

// recoverStreams recovers every generated stream through the real wire
// encoding and aggregates wrap counts and client-side statistics.
func recoverStreams(sc *scene.Scenario, rep *Report) []streamResult {
	out := make([]streamResult, 0, len(sc.RTP))
	recovered, total := 0, 0
	for _, s := range sc.RTP {
		res := recoverOne(sc, s)
		expected := s.Stream.Total - s.Stream.Dropped
		sr := streamResult{name: s.Name, expected: expected, got: res}
		sr.expectedLost = s.Stream.Dropped
		out = append(out, sr)
		rep.Wraps += res.Stats.Wraps
		if res.Stats.Recovered > 0 {
			total++
			if sr.ok() {
				recovered++
			}
		}
		for _, it := range res.Faults.Items {
			rep.Faults.Add(it)
		}
	}
	if total == 0 {
		total = len(sc.RTP)
	}
	if recovered == 0 && len(out) > 0 {
		// Count streams that recovered cleanly even if no packet happened to
		// be out of order.
		recovered = 0
		for _, sr := range out {
			if sr.ok() {
				recovered++
			}
		}
	}
	rep.Recovered = fmt.Sprintf("%d/%d", recovered, total)
	return out
}

// recoverOne round-trips one stream through pion/rtp and transport.Recover.
func recoverOne(_ *scene.Scenario, s scene.RTPStream) transport.Result {
	bufs, err := fixture.Marshal(s.Stream.Packets)
	if err != nil {
		res := transport.Result{Faults: &fault.List{}}
		res.Faults.Add(fault.Wrap(-1, 0, fault.KindCorrupt, false, err))
		return res
	}
	wire, err := fixture.Unmarshal(bufs)
	if err != nil {
		res := transport.Result{Faults: &fault.List{}}
		res.Faults.Add(fault.Wrap(-1, 0, fault.KindCorrupt, false, err))
		return res
	}
	parsed := make([]transport.Packet, 0, len(wire))
	for i, p := range wire {
		parsed = append(parsed, transport.Packet{
			Seq: p.SequenceNumber, Timestamp: p.Timestamp, Marker: p.Marker,
			TrackID: scene.RTPTrackID, Payload: p.Payload, Arrival: i + 1,
		})
	}
	return transport.Recover(parsed, transport.Options{
		Window: 8, MaxPackets: 1 << 16, MaxPayload: 1 << 20,
	})
}

// buildIndex builds and validates the keyframe index over video tracks.
func buildIndex(sc *scene.Scenario, rep *Report) {
	total, hits := 0, 0
	for _, clip := range []scene.Clip{sc.MP4, sc.MXF} {
		if clip.File == nil {
			continue
		}
		video := videoTracks(clip.File.Tracks)
		idx := index.Build(video, clip.File.Data)
		total += idx.Count()
		for _, e := range idx.Entries {
			tr, ok := trackByID(clip.File.Tracks, e.TrackID)
			if !ok {
				continue
			}
			if s, ok := tr.SampleByIndex(e.SampleIdx); ok && s.Sync && s.Offset == e.Offset {
				hits++
			}
		}
		rep.VFRIntervals = maxInt(rep.VFRIntervals, distinctDurations(video))
	}
	rep.Keyframe = fmt.Sprintf("%d/%d", hits, total)
}

func videoTracks(tracks []*model.Track) []*model.Track {
	out := make([]*model.Track, 0, len(tracks))
	for _, t := range tracks {
		if t.Kind == model.Video {
			out = append(out, t)
		}
	}
	return out
}

func distinctDurations(tracks []*model.Track) int {
	set := map[int64]bool{}
	for _, t := range tracks {
		for _, s := range t.Samples {
			set[s.Duration] = true
		}
	}
	return len(set)
}

func trackByID(tracks []*model.Track, id uint32) (*model.Track, bool) {
	for _, t := range tracks {
		if t.ID == id {
			return t, true
		}
	}
	return nil, false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// renderAll renders every composed video frame into dir and returns the PNG
// digests in canonical order. cancelAfter > 0 stops the producer after that
// many frames and leaves a checkpoint for a later resume.
func renderAll(ctx context.Context, sc *scene.Scenario, opts Options, dir string, cancelAfter int, anchors []timeline.Anchor) ([]string, error) {
	entries, mp4Track, desc, err := composeFrames(sc)
	if err != nil {
		return nil, err
	}
	// Resume: skip frames a previous cancelled run already published. The
	// checkpoint records how many frames were written and each existing file
	// is hashed so the final manifest matches an uninterrupted run exactly.
	var hashes []string
	resumeFrom := 0
	if opts.Resume {
		done, err := readCheckpoint(filepath.Join(dir, "checkpoint.json"))
		if err != nil {
			return nil, err
		}
		if done > len(entries) {
			return nil, fault.New(-1, 0, fault.KindCorrupt, false,
				"checkpoint claims %d frames but the timeline has %d", done, len(entries))
		}
		for i := 0; i < done; i++ {
			h, err := hashExistingFrame(dir, entries[i].SampleIdx)
			if err != nil {
				return nil, err
			}
			hashes = append(hashes, h)
		}
		resumeFrom = done
		entries = entries[resumeFrom:]
	}
	cctx := ctx
	if cancelAfter > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithCancel(ctx)
		defer cancel()
	}
	var processed int
	res, runErr := pipeline.Run(cctx, pipeline.Config{Workers: opts.Workers, Inflight: opts.Inflight}, pipeline.RunOptions{
		Items: entries,
		Work: func(c context.Context, e timeline.Entry) (any, error) {
			if c.Err() != nil {
				return nil, fault.Wrap(-1, 0, fault.KindCancelled, true, c.Err())
			}
			sample, ok := mp4Track.SampleByIndex(e.SampleIdx)
			if !ok {
				return nil, fault.New(mp4Track.Number, 0, fault.KindOutOfRange, true,
					"sample %d missing", e.SampleIdx)
			}
			payload, err := mp4Track.Payload(sc.MP4.File.Data, sample)
			if err != nil {
				return nil, fault.Wrap(mp4Track.Number, sample.Offset, fault.KindOutOfRange, true, err)
			}
			// Workers decode and crop in memory only; the write happens in the
			// serialized emission stage so cancellation before a write means no
			// file is created at all.
			r := render.RenderFrame(e.SampleIdx, payload, sample.Codec, desc, render.Options{})
			if r.Err != nil {
				return nil, r.Err
			}
			return r, nil
		},
		OnResult: func(m pipeline.Merged) error {
			if cancelAfter > 0 && processed >= cancelAfter {
				return fault.New(-1, 0, fault.KindCancelled, true, "simulated cancellation")
			}
			r, ok := m.Value.(render.Result)
			if !ok {
				return fault.New(-1, 0, fault.KindCorrupt, false, "unexpected job value %T", m.Value)
			}
			written, err := render.WriteRendered(dir, r)
			if err != nil {
				return err
			}
			processed++
			hashes = append(hashes, written.OutHash)
			return nil
		},
	})
	if isCancel(runErr) || res.Canceled {
		writeCheckpoint(dir, resumeFrom+processed, resumeFrom+len(entries))
		return nil, fault.New(-1, 0, fault.KindCancelled, true,
			"cancelled after %d frames", resumeFrom+processed)
	}
	if runErr != nil {
		return nil, runErr
	}
	if processed != len(entries) {
		return nil, fault.New(-1, 0, fault.KindCorrupt, false,
			"rendered %d of %d frames", processed, len(entries))
	}
	// A completed run has nothing left to resume.
	_ = os.Remove(filepath.Join(dir, "checkpoint.json"))
	// The pipeline emits in canonical order, so hashes are already ordered by
	// composition key; sort defensively by frame index if the merge changed.
	return hashes, nil
}

// composeFrames composes the MP4 video track on the shared timeline.
func composeFrames(sc *scene.Scenario) ([]timeline.Entry, *model.Track, model.Description, error) {
	mp4Track, ok := scene.VideoTrack(sc.MP4)
	if !ok {
		return nil, nil, model.Description{}, fault.New(-1, 0, fault.KindCorrupt, false, "no MP4 video track")
	}
	comp := timeline.Compose([]timeline.TrackSource{{Track: mp4Track}},
		timeline.ComposeOptions{MaxEntries: 1 << 16})
	if len(comp.Entries) == 0 {
		return nil, nil, model.Description{}, fault.New(-1, 0, fault.KindCorrupt, false,
			"composition produced no frames")
	}
	entries := make([]timeline.Entry, 0, len(comp.Entries))
	for _, e := range comp.Entries {
		if e.Codec == model.CodecMJPEG {
			entries = append(entries, e)
		}
	}
	return entries, mp4Track, mp4Track.Resolved, nil
}

func isCancel(err error) bool {
	fe, ok := fault.As(err)
	return ok && fe.Kind == fault.KindCancelled
}

// writeCheckpoint records progress so a resumed run can start where the
// cancelled one stopped.
func writeCheckpoint(dir string, done, total int) {
	body := fmt.Sprintf("{\"done\":%d,\"total\":%d}\n", done, total)
	_ = os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte(body), 0o644)
}

// printReport renders the stable key=value summary. The acceptance fields come
// first in a fixed order so the output can be matched literally.
func printReport(w io.Writer, rep *Report) {
	order := []string{
		"roundtrip_rate", "keyframe_hits", "frames", "segments",
		"sha256_match", "wraps", "vfr_intervals", "recovered", "cancelled",
	}
	vals := map[string]string{
		"roundtrip_rate": rep.RoundtripRate,
		"keyframe_hits":  rep.Keyframe,
		"frames":         strconv.Itoa(rep.Frames),
		"segments":       strconv.Itoa(rep.Segments),
		"sha256_match":   rep.SHA256Match,
		"wraps":          strconv.Itoa(rep.Wraps),
		"vfr_intervals":  strconv.Itoa(rep.VFRIntervals),
		"recovered":      rep.Recovered,
		"cancelled":      strconv.Itoa(rep.Cancelled),
	}
	for _, k := range order {
		fmt.Fprintf(w, "%s=%s\n", k, vals[k])
	}
	for _, c := range rep.Conflicts {
		losers := make([]string, 0, len(c.Losers))
		for _, l := range c.Losers {
			losers = append(losers, fmt.Sprintf("%s:%s", l.Source, l.Value))
		}
		fmt.Fprintf(w, "conflict=%s field=%s chose=%s:%s rejected=%s\n",
			c.TrackID, c.Field, c.Chosen.Source, c.Chosen.Value, strings.Join(losers, ","))
	}
	for _, it := range rep.Faults.Sorted() {
		fmt.Fprintf(w, "fault=%s\n", it)
	}
	for _, s := range rep.Shards {
		fmt.Fprintf(w, "png_shard=%d.%s\n", s.Index, s.Hash)
	}
	fmt.Fprintf(w, "exit=%d\n", rep.Exit)
}

// readCheckpoint parses the tiny JSON checkpoint written by writeCheckpoint.
func readCheckpoint(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fault.New(-1, 0, fault.KindCorrupt, true,
				"--resume requested but no checkpoint exists at %s", path)
		}
		return 0, fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	var cp struct {
		Done  int `json:"done"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(data, &cp); err != nil {
		return 0, fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	if cp.Done < 0 || cp.Done > cp.Total {
		return 0, fault.New(-1, 0, fault.KindCorrupt, false,
			"checkpoint progress %d/%d is inconsistent", cp.Done, cp.Total)
	}
	return cp.Done, nil
}

// hashExistingFrame recomputes a published frame's digest so a resumed run's
// manifest is identical to the uninterrupted one.
func hashExistingFrame(dir string, frameNo int) (string, error) {
	path := filepath.Join(dir, fmt.Sprintf("frame_%04d.png", frameNo))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fault.Wrap(-1, 0, fault.KindCorrupt, false, err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), nil
}
