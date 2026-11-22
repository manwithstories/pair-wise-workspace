package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"mediaslice/internal/fixture"
	"mediaslice/media"
	"mediaslice/stream"
)

// runDemo drives the full acceptance scenario end to end.
func runDemo(args []string) int {
	d, err := parseDemoFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parameter error: %v\n", err)
		return exitInternal
	}
	for _, inj := range d.inject {
		if !fixture.SupportsInjection(inj) {
			fmt.Fprintf(os.Stderr, "parameter error: unknown injection %q\n", inj)
			return exitInternal
		}
	}

	root, err := filepath.Abs(d.workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "io error: %v\n", err)
		return exitInternal
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "io error: %v\n", err)
		return exitInternal
	}
	runDir := filepath.Join(root, "primary")
	verifyDir := filepath.Join(root, "verify")
	for _, dir := range []string{runDir, verifyDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "io error: %v\n", err)
			return exitInternal
		}
	}

	segmentTicks := media.FromSeconds(d.segment.Seconds())
	durationTicks := media.FromSeconds(d.duration.Seconds())
	checkpointEverySeg := int(media.FromSeconds(d.checkpointEvery.Seconds()) / segmentTicks)
	if checkpointEverySeg < 1 {
		checkpointEverySeg = 1
	}

	// 1. Generate the healthy ladder fixtures (wrap + drift + VFR are native
	// to every fixture, so wrap-drift-vfr is exercised unconditionally).
	inputs := make([]pipelineInputs, 0, len(d.bitrates))
	plans := map[string]*fixture.Plan{}
	for i, id := range d.bitrates {
		spec := fixture.DefaultSpec(id, d.seed+int64(i), durationTicks, segmentTicks)
		spec.JitterSeed = d.seed // shared boundary clock across the ladder
		plan := fixture.BuildPlan(spec)
		mp4Path := filepath.Join(runDir, id+".mp4")
		rtpPath := filepath.Join(runDir, id+".rtp")
		if err := fixture.WriteMP4(mp4Path, plan); err != nil {
			fmt.Fprintf(os.Stderr, "io error: %v\n", err)
			return exitInternal
		}
		if err := fixture.WriteRTPSidecar(rtpPath, plan); err != nil {
			fmt.Fprintf(os.Stderr, "io error: %v\n", err)
			return exitInternal
		}
		inputs = append(inputs, pipelineInputs{id: id, mp4Path: mp4Path, rtpPath: rtpPath})
		plans[id] = plan
	}

	// 2. Optional corruption path: truncate an extra, isolated representation
	// so healthy ladder boundaries are never polluted.
	var corruptOffset int64 = -1
	var locateBytes int
	wantTruncated := contains(d.inject, fixture.InjectTruncatedBox)
	if wantTruncated {
		badID := "corrupt"
		badSpec := fixture.DefaultSpec(badID, d.seed+999, durationTicks, segmentTicks)
		badSpec.JitterSeed = d.seed
		badPlan := fixture.BuildPlan(badSpec)
		badMP4 := filepath.Join(runDir, badID+".mp4")
		if err := fixture.WriteMP4(badMP4, badPlan); err != nil {
			fmt.Fprintf(os.Stderr, "io error: %v\n", err)
			return exitInternal
		}
		off, err := fixture.TruncateBox(badMP4, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "io error: %v\n", err)
			return exitInternal
		}
		corruptOffset = off
		badIdx, err := scanFileLight(badMP4)
		if err != nil {
			fmt.Fprintf(os.Stderr, "io error: %v\n", err)
			return exitInternal
		}
		if e := badIdx.Errors.First(media.KindTruncatedBox); e != nil {
			locateBytes = e.ErrorBytes
			fmt.Printf("corrupt_offset=%d\n", e.ByteOffset)
			fmt.Printf("locate_error_bytes<=%d\n", media.MaxLocateErrorBytes)
		}
		if locateBytes > media.MaxLocateErrorBytes {
			return exitCorrupt
		}
		_ = corruptOffset
	}

	// 3. Build the aligned plan from healthy representations only.
	waitTicks := segmentTicks // never wait longer than one segment
	built, err := buildPlan(runDir, inputs, segmentTicks, waitTicks)
	if err != nil {
		return classifyError(err)
	}
	if built.plan == nil {
		fmt.Fprintf(os.Stderr, "corrupt input: no sliceable representation\n")
		return exitCorrupt
	}

	// 4. Execute (with optional crash + resume).
	cfg := stream.DefaultConfig()
	if err := executePlan(runDir, built, cfg, d.resume, checkpointEverySeg); err != nil {
		return classifyError(err)
	}
	rr := built.runnerResult

	// 5. Determinism re-run into an independent directory from fresh fixtures.
	verifyInputs := make([]pipelineInputs, 0, len(inputs))
	for i, id := range d.bitrates {
		spec := fixture.DefaultSpec(id, d.seed+int64(i), durationTicks, segmentTicks)
		spec.JitterSeed = d.seed
		plan := fixture.BuildPlan(spec)
		mp4Path := filepath.Join(verifyDir, id+".mp4")
		rtpPath := filepath.Join(verifyDir, id+".rtp")
		if err := fixture.WriteMP4(mp4Path, plan); err != nil {
			fmt.Fprintf(os.Stderr, "io error: %v\n", err)
			return exitInternal
		}
		if err := fixture.WriteRTPSidecar(rtpPath, plan); err != nil {
			fmt.Fprintf(os.Stderr, "io error: %v\n", err)
			return exitInternal
		}
		verifyInputs = append(verifyInputs, pipelineInputs{id: id, mp4Path: mp4Path, rtpPath: rtpPath})
	}
	verifyBuilt, err := buildPlan(verifyDir, verifyInputs, segmentTicks, waitTicks)
	if err != nil {
		return classifyError(err)
	}
	if err := executePlan(verifyDir, verifyBuilt, stream.DefaultConfig(), false, checkpointEverySeg); err != nil {
		return classifyError(err)
	}

	// 6. Acceptance output.
	fmt.Printf("segments=%d\n", rr.Manifest.SegmentCount)
	fmt.Printf("representations=%d\n", len(rr.Manifest.Representations))
	fmt.Printf("max_boundary_delta_ticks=%d\n", built.align.MaxDelta)
	manifestHash := hashManifest(rr.Manifest)
	fmt.Printf("manifest_sha256=%s\n", manifestHash)
	fmt.Printf("output_sha256=%s\n", rr.OutputSHA256)
	if d.resume {
		fmt.Printf("resume_ms=%d\n", built.resumeMS)
		fmt.Printf("reused_segments=%d\n", rr.ReusedSegments)
	}

	// 7. Deterministic verification: identical per-segment checksums and
	// matching hashes across the two independent runs.
	if verifyBuilt.runnerResult.OutputSHA256 != rr.OutputSHA256 {
		fmt.Fprintf(os.Stderr,
			"resume validation failed: output hash mismatch %s != %s\n",
			verifyBuilt.runnerResult.OutputSHA256, rr.OutputSHA256)
		return exitCorrupt
	}
	if hashManifest(verifyBuilt.runnerResult.Manifest) != manifestHash {
		fmt.Fprintf(os.Stderr, "resume validation failed: manifest hash mismatch\n")
		return exitCorrupt
	}
	if !manifestSegmentsEqual(rr.Manifest, verifyBuilt.runnerResult.Manifest) {
		fmt.Fprintf(os.Stderr, "resume validation failed: per-segment checksum mismatch\n")
		return exitCorrupt
	}
	if d.resume && (built.resumeMS >= 5000 || rr.ReusedSegments <= 0) {
		fmt.Fprintf(os.Stderr, "resume validation failed: resume_ms or reuse threshold\n")
		return exitCorrupt
	}
	if built.align.MaxDelta > 1 {
		fmt.Fprintf(os.Stderr, "resume validation failed: boundary delta exceeds one tick\n")
		return exitCorrupt
	}
	fmt.Println("status=ok")
	return exitOK
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func hashManifest(m *stream.Manifest) string {
	h := sha256.New()
	for _, e := range m.Segments {
		fmt.Fprintf(h, "%s/%d:%s:%d\n", e.RepID, e.Sequence, e.SHA256, e.Bytes)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func manifestSegmentsEqual(a, b *stream.Manifest) bool {
	if len(a.Segments) != len(b.Segments) {
		return false
	}
	for i := range a.Segments {
		if a.Segments[i].SHA256 != b.Segments[i].SHA256 ||
			a.Segments[i].RepID != b.Segments[i].RepID ||
			a.Segments[i].Sequence != b.Segments[i].Sequence {
			return false
		}
	}
	return true
}

func classifyError(err error) int {
	if me, ok := err.(*media.MediaError); ok {
		switch me.Kind {
		case media.KindTruncatedBox, media.KindBadSampleTable,
			media.KindMissingTrack, media.KindEditList,
			media.KindTimestampWrap, media.KindCheckpoint, media.KindUnsliceable:
			fmt.Fprintf(os.Stderr, "corrupt input or resume failure: %v\n", me)
			return exitCorrupt
		}
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return exitInternal
}
