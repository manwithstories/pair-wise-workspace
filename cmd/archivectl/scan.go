package main

import (
	"flag"
	"fmt"
	"os"

	"archivectl/internal/clock"
	"archivectl/internal/core"
	"archivectl/internal/fixture"
	"archivectl/internal/index"
	"archivectl/internal/verify"
	"archivectl/internal/wavscan"
)

type scanInputs struct {
	report      *wavscan.Report
	clockFile   *fixture.ClockFile
	checksFile  *fixture.ChecksFile
	crcSpans    []core.Span
	damageCount int
}

func runScan(args []string) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	wavPath := fs.String("wav", "", "path to WAV file")
	clockPath := fs.String("clock", "", "optional clock records JSON")
	checksPath := fs.String("checks", "", "optional block check records JSON")
	indexOut := fs.String("index-out", "", "optional path to write atomic index JSON")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	in, idx, err := buildIndexFromInputs(*wavPath, *clockPath, *checksPath, "")
	if err != nil {
		fmt.Printf("scan_status=failed\nscan_error=%q\n", err.Error())
		return core.ExitCode(err)
	}
	if *indexOut != "" {
		if err := idx.SaveAtomic(*indexOut); err != nil {
			fmt.Fprintf(os.Stderr, "index write failed: %v\n", err)
			return core.ExitCode(err)
		}
	}
	fmt.Printf("scan_status=ok\nwav=%s\n", *wavPath)
	printScanSummary(in, idx)
	if *indexOut != "" {
		fmt.Printf("index_path=%s\n", *indexOut)
	}
	return 0
}

func runIndex(args []string) int {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	wavPath := fs.String("wav", "", "path to WAV file")
	clockPath := fs.String("clock", "", "clock records JSON")
	checksPath := fs.String("checks", "", "block check records JSON")
	out := fs.String("out", "archive-index.json", "index output path")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	_, idx, err := buildIndexFromInputs(*wavPath, *clockPath, *checksPath, *out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "index failed: %v\n", err)
		return core.ExitCode(err)
	}
	if err := idx.SaveAtomic(*out); err != nil {
		fmt.Fprintf(os.Stderr, "index write failed: %v\n", err)
		return core.ExitCode(err)
	}
	fmt.Printf("index_status=ok\nindex_path=%s\nindex_version=%s\ndamage_spans=%d\n",
		*out, idx.IndexVersion, len(idx.Damage))
	return 0
}

func buildIndexFromInputs(wavPath, clockPath, checksPath, _ string) (*scanInputs, *index.Index, error) {
	if wavPath == "" {
		return nil, nil, core.New(core.KindInvalidArgument, "cli.scan", "--wav is required")
	}
	rep, err := wavscan.ScanPath(wavPath)
	if err != nil {
		return nil, nil, err
	}
	in := &scanInputs{report: rep}
	var anchors []clock.Anchor
	var blocks []clock.BlockRecord
	if clockPath != "" {
		cf, err := fixture.LoadClockFile(clockPath)
		if err != nil {
			return nil, nil, err
		}
		in.clockFile = cf
		blocks = cf.Blocks
		anchors = joinCLIAnchors(cf, rep.Cues)
	}
	var blockChecks []verify.BlockCheck
	if checksPath != "" {
		cf, err := fixture.LoadChecksFile(checksPath)
		if err != nil {
			return nil, nil, err
		}
		in.checksFile = cf
		blockChecks = cf.Blocks
		_, spans, err := verify.CheckBlocksPath(wavPath, rep.DataOffset,
			rep.Format.FrameBytes(), rep.AvailableFrames, cf.Blocks)
		if err != nil {
			return nil, nil, err
		}
		in.crcSpans = spans
	}
	idx, err := index.Build(index.BuildInput{
		WAVPath: wavPath, ClockPath: clockPath, ChecksPath: checksPath,
		Report: rep, Anchors: anchors, BlockRecords: blocks,
		BlockChecks: blockChecks, Damage: in.crcSpans,
	})
	if err != nil {
		return nil, nil, err
	}
	in.damageCount = len(idx.Damage)
	return in, idx, nil
}

func printScanSummary(in *scanInputs, idx *index.Index) {
	f := in.report.Format
	fmt.Printf("format=pcm channels=%d sample_rate=%d bits=%d frame_bytes=%d\n",
		f.Channels, f.SampleRate, f.BitsPerSample, f.FrameBytes())
	fmt.Printf("frames_declared=%d frames_available=%d cues=%d chunks=%d\n",
		in.report.DeclaredFrames, in.report.AvailableFrames, len(in.report.Cues), len(in.report.Chunks))
	unknown := 0
	for _, c := range in.report.Chunks {
		if c.Unknown {
			unknown++
		}
	}
	fmt.Printf("unknown_chunks_marked=%d damage_spans=%d normalized_anchors=%d\n",
		unknown, len(idx.Damage), len(idx.Anchors))
	for i, s := range idx.Damage {
		fmt.Printf("damage[%d]=%s frames=[%d,%d) count=%d recoverable=%t\n",
			i, s.Kind, s.StartFrame, s.EndFrame, s.Frames(), s.Recoverable)
	}
}

func joinCLIAnchors(cf *fixture.ClockFile, cues []wavscan.CuePoint) []clock.Anchor {
	tickByID := map[uint32]uint32{}
	for _, a := range cf.Anchors {
		tickByID[a.CueID] = a.Tick
	}
	var out []clock.Anchor
	seen := map[int64]bool{}
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
