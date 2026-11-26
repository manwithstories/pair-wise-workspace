// Package demo orchestrates the normal, damaged, repair, and verification
// paths used by archivectl demo.
package demo

import (
	"fmt"
	"path/filepath"

	"archivectl/internal/clock"
	"archivectl/internal/core"
	"archivectl/internal/fixture"
	"archivectl/internal/index"
	"archivectl/internal/repair"
	"archivectl/internal/verify"
	"archivectl/internal/wavscan"
)

// Options are the demo command parameters.
type Options struct {
	WorkDir  string
	Seed     int64
	Frames   int
	Channels int
	Inject   []string
}

// Report contains the machine-checkable demo metrics.
type Report struct {
	WorkDir          string
	ExitCode         int
	RoundtripMatched int
	RoundtripTotal   int
	KeyframeHits     int
	KeyframeTotal    int
	DamageSpans      int
	RecoveredFrames  int64
	SegmentMatched   int
	SegmentTotal     int
	FileSHA256       string
	RerunSHA256      string
	UnknownChunks    int
	RepairResult     *repair.Result
}

// Run generates local fixtures and executes all three paths.
func Run(opts Options) (*Report, error) {
	if opts.WorkDir == "" {
		opts.WorkDir = filepath.Join(".", "demo-work")
	}
	manifest, err := fixture.Generate(fixture.Config{
		Dir: opts.WorkDir, Seed: opts.Seed, Frames: opts.Frames,
		Channels: opts.Channels, SampleRate: 48000, Bits: 16,
		Injections: opts.Inject,
	})
	if err != nil {
		return nil, err
	}
	clockFile, err := fixture.LoadClockFile(manifest.ClockJSON)
	if err != nil {
		return nil, err
	}
	checksFile, err := fixture.LoadChecksFile(manifest.ChecksJSON)
	if err != nil {
		return nil, err
	}

	normalReport, err := wavscan.ScanPath(manifest.NormalWAV)
	if err != nil {
		return nil, err
	}
	normalAnchors := joinAnchors(clockFile, normalReport.Cues)
	normalIndex, err := index.Build(index.BuildInput{
		WAVPath: manifest.NormalWAV, ClockPath: manifest.ClockJSON, ChecksPath: manifest.ChecksJSON,
		Report: normalReport, Anchors: normalAnchors, BlockRecords: clockFile.Blocks,
	})
	if err != nil {
		return nil, err
	}
	normalIndexPath := filepath.Join(opts.WorkDir, "normal-index.json")
	if err := normalIndex.SaveAtomic(normalIndexPath); err != nil {
		return nil, err
	}
	roundtripPath := filepath.Join(opts.WorkDir, "normal-roundtrip.wav")
	if _, err := repair.Run(repair.Options{Index: normalIndex, OutputPath: roundtripPath}); err != nil {
		return nil, err
	}
	rtMatched, rtTotal, err := compareWAVSegments(manifest.NormalWAV, normalReport, roundtripPath)
	if err != nil {
		return nil, err
	}

	damagedReport, err := wavscan.ScanPath(manifest.DamagedWAV)
	if err != nil {
		return nil, err
	}
	_, crcSpans, err := verify.CheckBlocksPath(
		manifest.DamagedWAV, damagedReport.DataOffset, damagedReport.Format.FrameBytes(),
		damagedReport.AvailableFrames, checksFile.Blocks)
	if err != nil {
		return nil, err
	}
	damagedAnchors := joinAnchors(clockFile, damagedReport.Cues)
	damagedIndex, err := index.Build(index.BuildInput{
		WAVPath: manifest.DamagedWAV, ClockPath: manifest.ClockJSON, ChecksPath: manifest.ChecksJSON,
		Report: damagedReport, Anchors: damagedAnchors, BlockRecords: clockFile.Blocks,
		BlockChecks: checksFile.Blocks, Damage: crcSpans,
	})
	if err != nil {
		return nil, err
	}
	indexPath := filepath.Join(opts.WorkDir, "damaged-index.json")
	if err := damagedIndex.SaveAtomic(indexPath); err != nil {
		return nil, err
	}
	loadedIndex, err := index.Load(indexPath)
	if err != nil {
		return nil, err
	}

	hits, total := keyframeHits(manifest, damagedReport)
	unknown := 0
	for _, c := range damagedReport.Chunks {
		if c.Unknown {
			unknown++
		}
	}

	repairedPath := filepath.Join(opts.WorkDir, "repaired.wav")
	result, err := repair.Run(repair.Options{Index: loadedIndex, OutputPath: repairedPath})
	if err != nil {
		return nil, err
	}
	rerunPath := filepath.Join(opts.WorkDir, "repaired-rerun.wav")
	if _, err := repair.Run(repair.Options{Index: loadedIndex, OutputPath: rerunPath}); err != nil {
		return nil, err
	}
	firstDigest, err := verify.FileSHA256(repairedPath)
	if err != nil {
		return nil, err
	}
	rerunDigest, err := verify.FileSHA256(rerunPath)
	if err != nil {
		return nil, err
	}
	if firstDigest != rerunDigest {
		return nil, core.New(core.KindVerification, "demo.Run", "repair rerun produced different bytes")
	}

	repairedReport, err := wavscan.ScanPath(repairedPath)
	if err != nil {
		return nil, err
	}
	actual, err := verify.HashWAVPath(repairedPath, repairedReport.DataOffset,
		repairedReport.Format.FrameBytes(), repairedReport.DeclaredFrames)
	if err != nil {
		return nil, err
	}
	baselineReport, err := wavscan.ScanPath(manifest.BaselineWAV)
	if err != nil {
		return nil, err
	}
	expected, err := verify.HashWAVPath(manifest.BaselineWAV, baselineReport.DataOffset,
		baselineReport.Format.FrameBytes(), baselineReport.DeclaredFrames)
	if err != nil {
		return nil, err
	}
	comparison := verify.CompareSegments(actual, expected)
	if err := comparison.AllMatch(); err != nil {
		for _, r := range comparison.Results {
			if !r.Match {
				return nil, fmt.Errorf("%w: first mismatch segment %d", err, r.Index)
			}
		}
		return nil, err
	}

	exitCode := 0
	if len(loadedIndex.Damage) > 0 {
		exitCode = 2
	}
	return &Report{
		WorkDir: opts.WorkDir, ExitCode: exitCode,
		RoundtripMatched: rtMatched, RoundtripTotal: rtTotal,
		KeyframeHits: hits, KeyframeTotal: total,
		DamageSpans: len(loadedIndex.Damage), RecoveredFrames: result.RecoveredFrames,
		SegmentMatched: comparison.Matched, SegmentTotal: comparison.Total,
		FileSHA256: firstDigest, RerunSHA256: rerunDigest, UnknownChunks: unknown,
		RepairResult: result,
	}, nil
}

func compareWAVSegments(originalPath string, original *wavscan.Report, roundtripPath string) (int, int, error) {
	expected, err := verify.HashWAVPath(originalPath, original.DataOffset,
		original.Format.FrameBytes(), original.DeclaredFrames)
	if err != nil {
		return 0, 0, err
	}
	rtReport, err := wavscan.ScanPath(roundtripPath)
	if err != nil {
		return 0, 0, err
	}
	actual, err := verify.HashWAVPath(roundtripPath, rtReport.DataOffset,
		rtReport.Format.FrameBytes(), rtReport.DeclaredFrames)
	if err != nil {
		return 0, 0, err
	}
	c := verify.CompareSegments(actual, expected)
	return c.Matched, c.Total, c.AllMatch()
}

func joinAnchors(cf *fixture.ClockFile, cues []wavscan.CuePoint) []clock.Anchor {
	tickByID := make(map[uint32]uint32, len(cf.Anchors))
	for _, a := range cf.Anchors {
		tickByID[a.CueID] = a.Tick
	}
	var out []clock.Anchor
	seen := make(map[int64]bool)
	for _, c := range cues {
		tick, ok := tickByID[c.ID]
		if !ok || seen[c.Frame] {
			continue
		}
		seen[c.Frame] = true
		out = append(out, clock.Anchor{CueID: c.ID, Tick: tick, Frame: c.Frame})
	}
	return out
}

func keyframeHits(m *fixture.Manifest, r *wavscan.Report) (int, int) {
	expected := make(map[int64]bool)
	for f := 0; f < m.Frames; f += m.KeyframeInterval {
		expected[int64(f)] = true
	}
	actual := make(map[int64]bool)
	for _, c := range r.Cues {
		actual[c.Frame] = true
	}
	hits := 0
	for f := range expected {
		if actual[f] {
			hits++
		}
	}
	return hits, len(expected)
}
