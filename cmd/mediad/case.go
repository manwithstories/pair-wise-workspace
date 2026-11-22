package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"mediad/internal/audio"
	"mediad/internal/checkpoint"
	"mediad/internal/fault"
	"mediad/internal/fixture"
	segmenthash "mediad/internal/hash"
	"mediad/internal/probe"
	"mediad/internal/worker"
)

// OutputDir holds normalised PCM evidence inside each case directory.
const OutputDir = "out"

// runCase processes one generated case directory:
//  1. parse HLS and DASH, round trip both and score field preservation;
//  2. probe each segment, recording structural faults at exact offsets;
//  3. decode PCM audio, normalise it and write deterministic output bytes;
//  4. build the keyframe index and measure coverage of determinable
//     segment starts;
//  5. checkpoint in fixed-size chunks, resume, and prove the resumed pass
//     reproduces every valid output hash.
func runCase(ctx context.Context, b fixture.Batch, opts worker.Options, root string, resume bool) (caseReport, error) {
	rep := caseReport{Case: string(b.Case)}

	hls, err := readHLS(b.Dir)
	if err != nil {
		return rep, err
	}
	dash, err := readDASH(b.Dir)
	if err != nil {
		return rep, err
	}
	rep.RoundTripPct, err = roundTripScore(hls, dash)
	if err != nil {
		return rep, err
	}

	initData, err := os.ReadFile(filepath.Join(b.Dir, fixture.InitName))
	if err != nil {
		return rep, err
	}
	initInfo, _, err := probe.ParseInit(bytes.NewReader(initData), int64(len(initData)), fixture.InitName)
	if err != nil {
		return rep, err
	}
	timescales := map[uint32]uint32{}
	for _, tr := range initInfo.Tracks {
		timescales[tr.TrackID] = tr.Timescale
	}
	const channels = 2

	inputs := make([]worker.SegmentInput, 0, len(b.Segments))
	var expectedStarts []int64
	for i, seg := range b.Segments {
		ms := manifestSegment(hls, seg, i)
		media, fl := readMedia(seg, timescales)
		src := &audio.Source{URI: seg.URI, StartTicks: seg.StartTicks, DurationTicks: seg.Duration, Decodable: false}
		if media != nil && media.Determined && !media.Truncated {
			if decoded, derr := audio.DecodePCM(seg.Bytes, media, b.SampleRate, channels); derr == nil {
				src = &decoded
			}
		}
		if media != nil && media.Determined {
			for _, frag := range media.Fragments {
				if !frag.Determined || len(frag.Samples) == 0 {
					continue
				}
				if frag.Samples[0].Sync {
					expectedStarts = append(expectedStarts, media.StartTicks)
					break
				}
			}
		}
		var fls []fault.Fault
		if fl != nil {
			fls = fl.All()
		}
		inputs = append(inputs, worker.SegmentInput{
			Segment: ms, Media: media, Timescales: timescales, Audio: src,
			InputHash: segmenthash.Bytes(seg.Bytes), InputSize: int64(len(seg.Bytes)), ProbeFaults: fls,
		})
	}

	digest, err := hls.Digest()
	if err != nil {
		return rep, err
	}
	caseDir := filepath.Join(root, string(b.Case), "checkpoint")
	store, err := checkpoint.New(caseDir)
	if err != nil {
		return rep, err
	}
	// A full recompute is the evidence baseline. It never reads a
	// checkpoint, so the resumed pass can be compared against independent
	// work rather than against itself.
	full, err := worker.Run(ctx, inputs, opts, digest, nil)
	if err != nil {
		return rep, err
	}
	var prior *checkpoint.State
	if resume {
		prior, err = store.Load()
		if err != nil {
			return rep, err
		}
	}
	if prior == nil {
		prior, err = worker.CheckpointFromResult(full, opts, nil)
		if err != nil {
			return rep, err
		}
	}
	// Persist the completed checkpoint so a later --resume run skips it.
	stored, err := worker.CheckpointFromResult(full, opts, prior)
	if err != nil {
		return rep, err
	}
	if err := store.Save(stored); err != nil {
		return rep, err
	}
	resumed, err := worker.Run(ctx, inputs, opts, digest, prior)
	if err != nil {
		return rep, err
	}
	if len(resumed.Outputs) != len(full.Outputs) {
		return rep, fmt.Errorf("case %s: resumed %d of %d outputs", b.Case, len(resumed.Outputs), len(full.Outputs))
	}

	rep.SegmentsOK = full.SegmentsOK()
	rep.SegmentsBad = full.SegmentsBad()
	rep.RecoveredChunks = resumed.Recovered
	rep.TimelineGaps = full.TimelineGaps()
	for _, f := range full.Faults {
		if f.Kind == fault.KindTimestampDiscontinuity {
			rep.TimelineJumps++
		}
	}

	// Hash equality over valid (determined, non-gap) segments: each valid
	// segment's output digest must be identical after recovery.
	valid := 0
	matched := 0
	for i := range full.Outputs {
		a, r := full.Outputs[i], resumed.Outputs[i]
		if !a.Determined || a.Gap || !a.OutputKnown {
			continue
		}
		valid++
		if a.OutputHash == r.OutputHash && a.OutputSize == r.OutputSize {
			matched++
		}
	}
	rep.HashMatched, rep.HashTotal = matched, valid

	// Keyframe coverage: prefer the shipped index. When it is absent or
	// disagrees with the fragment scan, rebuild by scanning segments. The
	// denominator counts only keyframe positions the scan could determine.
	shipped, err := fixture.LoadIndex(b.Dir)
	if err != nil {
		return rep, err
	}
	idx := assembleIndex(shipped, full, b.Segments, inputs)
	rep.IndexRebuilt = idx.Rebuilt
	rep.KeyframeHits, rep.KeyframeTotal = keyframeCoverage(idx, expectedStarts)
	if rep.KeyframeTotal > 0 {
		rep.KeyframeHitPct = 100 * float64(rep.KeyframeHits) / float64(rep.KeyframeTotal)
	}
	rep.KeyframeUndetermined = idx.Undetermined()

	if err := writeOutputs(b.Dir, full, opts); err != nil {
		return rep, err
	}
	rtpSummary, err := writeRTPEvidence(b.Dir, full, opts.Target.LoudnessLUFS)
	if err != nil {
		return rep, err
	}
	rep.RTPPackets = len(rtpSummary.Packets)
	rep.RTPMonotonic = rtpSummary.Monotonic
	rep.SegmentHashes = segmentEvidenceFor(b, full, opts)
	return rep, nil
}
