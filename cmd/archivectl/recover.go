package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"archivectl/internal/container"
	"archivectl/internal/manifest"
	"archivectl/internal/timeline"
	"archivectl/internal/transport"
)

func runRecover(args []string) int {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	in := fs.String("in", "", "input MP4 or length-framed RTP capture")
	out := fs.String("out", "", "recovered MP4 output path")
	manifestPath := fs.String("manifest", "", "cross-track event CSV path")
	journalPath := fs.String("journal", "", "resume journal JSON path")
	targetKBPS := fs.Int("target-kbps", 1200, "target bitrate in kbps")
	window := fs.Int("window", 64, "bounded RTP reorder window")
	resume := fs.Bool("resume", false, "resume and verify against journal")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" || *out == "" || *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "recover: -in, -out and -manifest are required")
		return 2
	}

	metrics, j, err := executeRecover(recoverOptions{
		In: *in, Out: *out, Manifest: *manifestPath, Journal: *journalPath,
		TargetKBPS: *targetKBPS, Window: *window, Resume: *resume,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "recover failed: %v\n", err)
		return timeline.ExitCode(err)
	}
	printRecoverMetrics(metrics, j)
	if *journalPath != "" {
		fmt.Printf("journal=%s\n", *journalPath)
	}
	return metrics.code
}

type recoverOptions struct {
	In, Out, Manifest, Journal string
	TargetKBPS, Window         int
	Resume                     bool
}

func executeRecover(opts recoverOptions) (recoverMetrics, manifest.Journal, error) {
	m := recoverMetrics{code: 0, targetKBPS: opts.TargetKBPS}
	if _, err := timeline.ValidateDefaultGraph(); err != nil {
		return m, manifest.Journal{}, err
	}
	inputHash, err := manifest.HashFile(opts.In)
	if err != nil {
		return m, manifest.Journal{}, err
	}
	nowJournal := manifest.Journal{
		Version: 1, InputPath: opts.In, InputSHA256: inputHash,
		OutputPath: opts.Out, ManifestPath: opts.Manifest,
		Window: opts.Window, TargetKBPS: opts.TargetKBPS,
	}
	var oldJournal manifest.Journal
	if opts.Resume && opts.Journal != "" {
		old, err := manifest.LoadJournal(opts.Journal)
		if err == nil {
			oldJournal = old
			if err := manifest.ValidateResume(oldJournal, nowJournal); err != nil {
				return m, nowJournal, err
			}
		} else if !os.IsNotExist(err) {
			return m, nowJournal, err
		}
	}

	// Remove stale artifacts first so a structural failure cannot leave a file
	// that looks like a complete result from an earlier run.
	_ = os.Remove(opts.Out)
	_ = os.Remove(opts.Manifest)

	asset, win, err := readRecoverInput(opts.In, opts.Window)
	if err != nil {
		return m, nowJournal, err
	}
	if win != nil {
		m.dupDrop = win.Counters.DupDrop
		m.reordered = win.Counters.Reordered
		m.gaps = win.Counters.GapPackets
		m.gapRanges = win.Counters.GapRanges
		if win.HasGaps() {
			asset.Partial = true
			asset.GapPackets = m.gaps
			asset.GapRanges = m.gapRanges
			m.code = 2
		}
	}

	bw, err := bandwidthForAsset(asset, opts.TargetKBPS)
	if err != nil {
		return m, nowJournal, err
	}
	m.bandwidth = bw

	remux, err := container.RemuxAsset(asset)
	if err != nil {
		return m, nowJournal, err
	}
	outAsset, err := container.ParseBytes("remux-check.mp4", remux.Bytes)
	if err != nil {
		return m, nowJournal, err
	}
	fidelity, err := container.VerifyAssets(asset, outAsset)
	m.payloadMismatch = fidelity.PayloadHashMismatch
	m.byteCountMismatch = fidelity.ByteCountMismatch
	m.timestampMismatch = fidelity.TimestampMismatch
	m.sampleCount = fidelity.SampleCount
	m.payloadBytes = fidelity.PayloadBytes
	if err != nil {
		return m, nowJournal, err
	}

	events := container.CollectEvents(asset)
	entries := manifest.BuildEntries(events)
	maxMS, _, err := manifest.CheckEvents(entries, events)
	if err != nil {
		return m, nowJournal, err
	}
	m.manifestMaxErrorMS = maxMS
	csvBytes, err := manifest.EncodeCSV(entries)
	if err != nil {
		return m, nowJournal, err
	}
	m.outputHash = remux.OutputHash
	m.manifestHash = manifest.HashBytes(csvBytes)

	// Check a resumed checkpoint before creating any new output artifact.
	if opts.Resume && oldJournal.InputSHA256 != "" {
		if oldJournal.OutputSHA256 != m.outputHash || oldJournal.ManifestSHA256 != m.manifestHash {
			return m, nowJournal, timeline.New(timeline.CodeIntegrity, "resume", "recomputed artifacts differ from journal")
		}
	}
	if err := atomicWriteFile(opts.Manifest, csvBytes, 0o644); err != nil {
		return m, nowJournal, err
	}
	if err := atomicWriteFile(opts.Out, remux.Bytes, 0o644); err != nil {
		return m, nowJournal, err
	}
	readEntries, err := manifest.ReadCSV(opts.Manifest)
	if err != nil {
		return m, nowJournal, err
	}
	if _, _, err := manifest.CheckEvents(readEntries, events); err != nil {
		return m, nowJournal, err
	}
	j := nowJournal
	j.Complete = true
	j.Partial = m.code == 2
	j.ExitCode = m.code
	j.OutputSHA256 = m.outputHash
	j.ManifestSHA256 = m.manifestHash
	j.DupDrop = m.dupDrop
	j.Reordered = m.reordered
	j.Gaps = m.gaps
	j.GapRanges = m.gapRanges
	j.Fabricated = 0
	j.PayloadHashMismatch = m.payloadMismatch
	j.ActualKBPS = bw.ActualKBPS
	j.DeviationPct = bw.DeviationPct
	if win != nil {
		j.GapLedger = make([]manifest.GapRange, 0, len(win.Gaps))
		for _, g := range win.Gaps {
			j.GapLedger = append(j.GapLedger, manifest.GapRange{
				Start: g.Start, End: g.End, Count: g.Count, Reason: g.Reason,
			})
		}
	}
	if opts.Journal != "" {
		if err := manifest.SaveJournal(opts.Journal, j); err != nil {
			return m, j, err
		}
	}
	return m, j, nil
}

func readRecoverInput(in string, window int) (*container.Asset, *transport.ReorderWindow, error) {
	if strings.EqualFold(filepath.Ext(in), ".rtp") {
		f, err := os.Open(in)
		if err != nil {
			return nil, nil, timeline.Wrap(timeline.CodeInvalidArgument, "open rtp", err)
		}
		defer f.Close()
		frames, err := transport.ReadCapture(f)
		if err != nil {
			return nil, nil, err
		}
		segments, win, err := transport.AssembleCapture(frames, window)
		if err != nil {
			return nil, win, err
		}
		parts := make([]*container.Asset, 0, len(segments))
		for _, seg := range segments {
			a, err := container.ParseBytes(fmt.Sprintf("%s#segment-%d", in, seg.ID), seg.Data)
			if err != nil {
				return nil, win, err
			}
			parts = append(parts, a)
		}
		return container.MergeAssets(parts...), win, nil
	}
	a, err := container.ParseFile(in)
	return a, nil, err
}

func bandwidthForAsset(a *container.Asset, target int) (transport.BandwidthReport, error) {
	track := primaryTrack(a)
	segments := make([]transport.BandwidthSegment, 0, len(track.Samples))
	for _, s := range track.Samples {
		if s.Duration <= 0 {
			return transport.BandwidthReport{}, &timeline.Error{
				Code: timeline.CodeNegativeDuration, Op: "bandwidth duration", TrackID: track.ID, Offset: s.FileOffset,
				Detail: fmt.Sprintf("sample %d duration=%d", s.Index, s.Duration),
			}
		}
		ns, err := timeline.ToNanoseconds(s.Duration, track.Timescale)
		if err != nil {
			return transport.BandwidthReport{}, err
		}
		segments = append(segments, transport.BandwidthSegment{ID: s.Index, DurationNS: ns, Bytes: int64(s.Size)})
	}
	return transport.MeasureBandwidth(target, segments), nil
}

func primaryTrack(a *container.Asset) container.Track {
	for _, t := range a.Tracks {
		if t.Kind == container.KindVideo {
			return t
		}
	}
	if len(a.Tracks) > 0 {
		return a.Tracks[0]
	}
	return container.Track{Timescale: timeline.Nanoseconds}
}
