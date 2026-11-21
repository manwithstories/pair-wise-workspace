package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediapack/internal/audio"
	"mediapack/internal/clock"
	"mediapack/internal/color"
	"mediapack/internal/dag"
	"mediapack/internal/fault"
	"mediapack/internal/fixtures"
	"mediapack/internal/mp4"
	"mediapack/internal/segments"
)

// result is what the demo prints and how the process should exit.
type result struct {
	ExitCode int
}

// runState carries the per-invocation context.  Computed results live in
// artifacts on disk, not here, so that a skipped node and an executed node
// produce the same report.
type runState struct {
	opts   options
	bundle *fixtures.Bundle
	state  *dag.State

	frameTicks clock.Ticks

	// summaries caches MP4 parse results for stages that need them twice in
	// one invocation; it is never the source of truth for the report.
	summaries map[int]*mp4.Summary
	plan      *segments.Plan

	// run is the DAG result for this invocation, used by the report.
	run *dag.Result

	// reportManifest is loaded from the hash artifact after a successful
	// run, whether that run executed or skipped the hash node.
	reportManifest hashArtifact
}

func demo(opts options, w io.Writer) result {
	out := newPrinter(w)
	rs := &runState{
		opts:      opts,
		summaries: map[int]*mp4.Summary{},
	}

	frameTicks, err := clock.New(int64(opts.SegmentTick), int64(fixtures.DefaultTimescale))
	if err != nil {
		reportFailure(out, fault.Wrap(fault.ClassUsage, "clock", "*", "invalid-segment",
			"segment-ticks cannot be converted to a time base", err))
		return result{ExitCode: 1}
	}
	rs.frameTicks = frameTicks

	bundle, err := rs.generate()
	if err != nil {
		reportFailure(out, err)
		return result{ExitCode: fault.ExitCode(err)}
	}
	rs.bundle = bundle

	// --resume demonstrates interrupted work honestly: when no state exists
	// yet, the stages through "segments" are completed first and the
	// invocation then resumes the remainder.  The final hashes are compared
	// against a clean run at the end.
	if opts.Resume && !stateExists(opts.WorkDir) {
		if code := rs.primeInterruptedRun(out); code != 0 {
			return result{ExitCode: code}
		}
	}

	state, err := dag.Load(filepath.Join(opts.WorkDir, "state.json"))
	if err != nil {
		reportFailure(out, err)
		return result{ExitCode: fault.ExitCode(err)}
	}
	rs.state = state

	if err := rs.runGraph(); err != nil {
		reportFailure(out, err)
		printBlockedChain(out, rs.state)
		return result{ExitCode: fault.ExitCode(err)}
	}

	manifest, err := rs.manifestView(rs.configHash())
	if err != nil {
		reportFailure(out, err)
		return result{ExitCode: fault.ExitCode(err)}
	}
	rs.reportManifest = manifest
	if err := rs.restorePlan(); err != nil {
		reportFailure(out, err)
		return result{ExitCode: fault.ExitCode(err)}
	}

	rs.printReport(out)
	if err := rs.verifyResume(); err != nil {
		reportFailure(out, err)
		return result{ExitCode: fault.ExitCode(err)}
	}
	return result{ExitCode: 0}
}

func (rs *runState) generate() (*fixtures.Bundle, error) {
	return fixtures.Generate(fixtures.Options{
		Seed: rs.opts.Seed, Rates: rs.opts.Rates, FrameCount: rs.opts.FrameCount,
		Timescale: fixtures.DefaultTimescale, Delta: fixtures.DefaultFrameDelta,
		Dir: filepath.Join(rs.opts.WorkDir, "corpus"), Inject: injectFlags(rs.opts.Inject),
	})
}

// restorePlan loads the plan from its artifact when the segments node was
// skipped, so printReport can count segments in both paths.
func (rs *runState) restorePlan() error {
	_, err := ensurePlan(rs, rs.configHash())
	return err
}

func stateExists(workDir string) bool {
	_, err := os.Stat(filepath.Join(workDir, "state.json"))
	return err == nil
}

// primeInterruptedRun executes only the stages through "segments" and leaves
// the state file behind, simulating a run that died before hashing.  It must
// not print the success report: this is setup, not output.
func (rs *runState) primeInterruptedRun(out *printer) int {
	state, err := dag.Load(filepath.Join(rs.opts.WorkDir, "state.json"))
	if err != nil {
		reportFailure(out, err)
		return fault.ExitCode(err)
	}
	rs.state = state
	nodes, err := rs.buildNodes()
	if err != nil {
		reportFailure(out, err)
		return fault.ExitCode(err)
	}
	wanted := map[string]bool{"frames": true, "audio": true, "color": true, "mp4": true,
		"index": true, "segments": true}
	var partial []dag.Node
	for _, n := range nodes {
		if wanted[n.Name] {
			partial = append(partial, n)
		}
	}
	if _, err := dag.Run(partial, state, false); err != nil {
		reportFailure(out, err)
		return fault.ExitCode(err)
	}
	return 0
}

func injectFlags(name string) fixtures.Inject {
	switch name {
	case "corrupt-frame":
		return fixtures.Inject{CorruptFrame: true}
	case "duplicate-frame":
		return fixtures.Inject{DuplicateFrame: true}
	case "missing-frame":
		return fixtures.Inject{MissingFrame: true}
	case "truncated-mp4":
		return fixtures.Inject{TruncatedMP4: true}
	case "illegal-box":
		return fixtures.Inject{IllegalBoxSize: true}
	case "audio-conflict":
		return fixtures.Inject{AudioConflict: true}
	case "color-conflict":
		return fixtures.Inject{ColorConflict: true}
	case "misaligned-rate":
		return fixtures.Inject{MisalignedRate: true}
	case "unknown-box":
		return fixtures.Inject{UnknownMP4Box: true}
	default:
		return fixtures.Inject{}
	}
}

// runGraph wires the pipeline into the DAG.  Every node's key includes the
// fixture hashes and the configuration, so an identical rerun skips all work
// and a changed injection reruns exactly the affected nodes.
func (rs *runState) runGraph() error {
	nodes, err := rs.buildNodes()
	if err != nil {
		return err
	}
	res, err := dag.Run(nodes, rs.state, rs.opts.Resume)
	if err != nil {
		return err
	}
	rs.run = res
	if res.FirstBlock != nil {
		return res.FirstBlock
	}
	return nil
}

// verifyResume proves that a resumed run reproduced the uninterrupted run.
// The reference manifest is regenerated from the same fixtures in a scratch
// directory; the two are compared segment by segment.
func (rs *runState) verifyResume() error {
	if !rs.opts.Resume {
		return nil
	}
	ref := &runState{
		opts: options{Seed: rs.opts.Seed, Rates: rs.opts.Rates, FrameCount: rs.opts.FrameCount,
			SegmentTick: rs.opts.SegmentTick, Policies: rs.opts.Policies,
			WorkDir: filepath.Join(rs.opts.WorkDir, "reference")},
		summaries: map[int]*mp4.Summary{},
	}
	ticks, err := clock.New(int64(rs.opts.SegmentTick), int64(fixtures.DefaultTimescale))
	if err != nil {
		return err
	}
	ref.frameTicks = ticks
	bundle, err := ref.generate()
	if err != nil {
		return err
	}
	ref.bundle = bundle
	state, err := dag.Load(filepath.Join(ref.opts.WorkDir, "state.json"))
	if err != nil {
		return err
	}
	ref.state = state
	if err := ref.runGraph(); err != nil {
		return err
	}
	refManifest, err := ref.manifestView(ref.configHash())
	if err != nil {
		return err
	}
	if !hashArtifactsMatch(rs.reportManifest, refManifest) {
		return fault.New(fault.ClassInternal, "manifest", "*", "resume-diverged",
			"resumed run produced different segment hashes than an uninterrupted run")
	}
	return nil
}

func hashArtifactsMatch(a, b hashArtifact) bool {
	if len(a.Segments) != len(b.Segments) {
		return false
	}
	for i := range a.Segments {
		if a.Segments[i].Ordinal != b.Segments[i].Ordinal || a.Segments[i].SHA256 != b.Segments[i].SHA256 {
			return false
		}
	}
	return true
}

func (rs *runState) configHash() string {
	payload := fmt.Sprintf("seed=%d;rates=%v;ticks=%d;frames=%d;inject=%s;audio=%s;color=%s",
		rs.opts.Seed, rs.opts.Rates, rs.opts.SegmentTick, rs.opts.FrameCount,
		rs.opts.Inject, rs.opts.Policies.Audio, rs.opts.Policies.Color)
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// fixtureInputs returns one stable hash per representation.
func (rs *runState) fixtureInputs() []string {
	var out []string
	for _, r := range rs.bundle.Rates {
		out = append(out, r.FileHash)
	}
	return out
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func jsonBytes(v any) []byte {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// audioConfig and colorConfig parse the CLI policy strings into stage config.
func audioConfig(s string) audio.Config {
	switch {
	case s == "" || s == "block":
		return audio.Config{Policy: audio.PolicyBlock}
	case len(s) > 7 && s[:7] == "prefer:":
		return audio.Config{Policy: audio.PolicyPrefer, PreferSource: s[7:]}
	case len(s) > 8 && s[:8] == "downmix:":
		return audio.Config{Policy: audio.PolicyMapDown, Target: audio.ParseLayout(s[8:])}
	}
	return audio.Config{Policy: audio.PolicyBlock}
}

func colorConfig(s string) color.Config {
	switch {
	case s == "" || s == "block":
		return color.Config{Policy: color.PolicyBlock}
	case len(s) > 7 && s[:7] == "prefer:":
		return color.Config{Policy: color.PolicyPrefer, PreferSource: s[7:]}
	}
	return color.Config{Policy: color.PolicyBlock}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
