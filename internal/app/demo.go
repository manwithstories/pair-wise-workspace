package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	rtpadapter "mediaproc/internal/rtp"

	"mediaproc/internal/audio"
	"mediaproc/internal/demux"
	"mediaproc/internal/fingerprint"
	"mediaproc/internal/model"
	"mediaproc/internal/pipeline"
	"mediaproc/internal/sample"
	"mediaproc/internal/segment"
	"mediaproc/internal/verify"
)

// RunDemo executes the deterministic generation, parse, cut, analyze, dedup,
// verify and bounded-publish flow.
func RunDemo(opts Options) (*Report, error) {
	if opts.Out == "" {
		opts.Out = filepath.Join(os.TempDir(), "mediaproc-demo")
	}
	if opts.Queue <= 0 {
		opts.Queue = 4
	}
	if opts.MaxMemory <= 0 {
		opts.MaxMemory = 8 << 20
	}
	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return nil, err
	}

	fixture := sample.Generate(sample.Config{Seed: opts.Seed, Fault: opts.Fault})
	inputPath := filepath.Join(opts.Out, "input.webm")
	if err := os.WriteFile(inputPath, fixture.Data, 0o644); err != nil {
		return nil, err
	}
	file, err := os.Open(inputPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	parser, err := demux.NewParser(file, demux.DefaultConfig())
	if err != nil {
		return nil, err
	}
	if _, err := parser.ReadMetadata(); err != nil {
		return nil, err
	}

	cutter := segment.NewCutter(segment.DefaultOptions())
	detector := audio.NewDetector()
	fpStore := fingerprint.NewStore()
	rtpPack := rtpadapter.NewOpusPacketizer(0x4d50524f)
	pub, err := newPublisher(opts.Out)
	if err != nil {
		return nil, err
	}
	runner := pipeline.NewRunner(opts.Queue, opts.MaxMemory, opts.SlowConsumer)
	runner.StartGate = make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-runner.StartGate:
				return
			case <-ticker.C:
				if runner.Metrics().PeakQueue >= opts.Queue {
					runner.Resume()
					return
				}
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer runner.Resume()
	runErr := make(chan error, 1)
	go func() { runErr <- runner.Run(ctx, pub.Publish) }()

	report := &Report{MemoryLimit: opts.MaxMemory}
	var segments []*segment.Segment
	var firstFault *model.Error
	keyframes, keyframeHits := 0, 0

	processSegment := func(s *segment.Segment) error {
		for _, w := range detector.Windows {
			if w.StartNS >= s.StartNS && w.StartNS < s.EndNS && w.Silent {
				s.SilentWindows++
			}
		}
		var removed int
		s.Blocks, removed = fpStore.DedupSegment(s.Blocks)
		report.Dedup += removed
		s.SHA256 = segment.Hash(s)
		if err := verify.VerifySegmentHash(s); err != nil {
			return err
		}
		s.MP4Validated = false
		for _, b := range s.Blocks {
			if b != nil && b.Track != nil && verify.SupportedMP4Codec(b.Track.CodecID) {
				mp4, err := verify.BuildMP4Sidecar(uint32(s.Index), s.StartNS, []byte(s.SHA256))
				if err != nil {
					return model.InvariantError(model.PhaseMP4, s.KeyframeOffset, "build sidecar", err)
				}
				if err := verify.ValidateMP4Sidecar(mp4); err != nil {
					return err
				}
				s.MP4Validated = true
				break
			}
		}
		for _, b := range s.Blocks {
			if b != nil && b.Track != nil && b.Track.CodecID == "A_OPUS" {
				pkts, perr := rtpPack.Packetize(b)
				if perr != nil {
					return perr
				}
				report.RTPPackets += len(pkts)
			}
		}
		segments = append(segments, s)
		return runner.Submit(ctx, pipeline.Item{
			ID:      s.SHA256[:16],
			Offset:  s.KeyframeOffset,
			Segment: s,
		})
	}

loop:
	for {
		b, err := parser.NextBlock()
		if errors.Is(err, io.EOF) {
			break loop
		}
		if err != nil {
			fe, ok := model.AsError(err)
			if opts.Recover && ok && fe.Recoverable {
				if firstFault == nil {
					firstFault = fe
					report.FirstErrorOffset = fe.Offset
				}
				boundary, rerr := parser.Recover(err)
				if rerr != nil {
					cancel()
					return report, rerr
				}
				report.Recoveries++
				report.RecoveryOffsets = append(report.RecoveryOffsets, boundary)
				continue
			}
			cancel()
			return report, err
		}

		if b.Keyframe {
			keyframes++
			planned := model.Nanoseconds(math.Round(float64(b.TimeNS)/float64(200*model.Millisecond))) *
				(200 * model.Millisecond)
			delta := b.TimeNS - planned
			if delta < 0 {
				delta = -delta
			}
			if delta <= 100*model.Millisecond {
				keyframeHits++
			}
		}
		detector.AnalyzeBlock(b)
		if closed := cutter.Add(b); closed != nil {
			if err := processSegment(closed); err != nil {
				cancel()
				return report, err
			}
		}
	}

	if last := cutter.Flush(); last != nil {
		if err := processSegment(last); err != nil {
			cancel()
			return report, err
		}
	}
	runner.Resume()
	runner.Close()
	if err := <-runErr; err != nil {
		return report, err
	}

	hashResult, err := verify.VerifySegments(segments)
	if err != nil {
		return report, err
	}
	records := pub.Records()
	if _, err := verify.RoundtripRecords(records); err != nil {
		return report, err
	}
	if _, err := pub.WriteManifest(); err != nil {
		return report, err
	}
	if ok, total, err := CheckManifest(opts.Out); err != nil || ok != total {
		if err != nil {
			return report, err
		}
		return report, model.InvariantError(model.PhaseVerify, -1, "manifest hash mismatch", nil)
	}

	m := runner.Metrics()
	report.Segments = len(segments)
	report.HashOK = hashResult.OK
	report.HashTotal = hashResult.Total
	report.RoundtripPercent = verify.RoundtripPercent(hashResult.OK, hashResult.Total)
	report.Silence = detector.SilentWindows
	report.Dedup = fpStore.Duplicates
	report.PeakQueue = m.PeakQueue
	report.PeakMemory = m.PeakMemory
	report.TrackErrors = len(detector.TrackErrors)
	if keyframes == 0 {
		report.KeyframePercent = "100.00%"
	} else {
		report.KeyframePercent = formatPercent(keyframeHits, keyframes)
	}
	if firstFault != nil {
		report.FirstErrorOffset = firstFault.Offset
	}
	report.ExitCode = 0
	if err := writeReport(opts.Out, report); err != nil {
		return report, err
	}
	return report, nil
}

func formatPercent(ok, total int) string {
	return fmt.Sprintf("%.2f%%", 100*float64(ok)/float64(total))
}

func writeReport(out string, r *Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(out, "report.json"), data, 0o644)
}
