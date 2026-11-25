package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mediad/internal/demo"
	"mediad/internal/fixture"
	"mediad/internal/ingest"
	"mediad/internal/mediaerr"
	"mediad/internal/pipeline"
	"mediad/internal/resume"
	"mediad/internal/verify"
)

type demoOptions struct {
	seed           int64
	dir            string
	faults         string
	interruptAfter int
	resume         bool
}

func runDemo(opts demoOptions) int {
	if opts.dir == "" {
		return usageError("--dir is required")
	}
	if opts.faults != "all" && opts.faults != "none" {
		return usageError("--faults must be \"all\" or \"none\"")
	}
	if opts.interruptAfter < 0 || opts.interruptAfter > 100000 {
		return usageError("--interrupt-after is out of bounds")
	}
	if err := os.MkdirAll(opts.dir, 0o755); err != nil {
		return operationalError(err)
	}

	specs := fixture.DemoRenditionSpecs()
	var clean [2][]byte
	var sourcePaths [2]string
	for i, spec := range specs {
		b, err := fixture.CleanRendition(opts.seed, spec)
		if err != nil {
			return operationalError(err)
		}
		clean[i] = b
		sourcePaths[i] = filepath.Join(opts.dir, fmt.Sprintf("rendition_%d.ts", i))
		if err := os.WriteFile(sourcePaths[i], b, 0o644); err != nil {
			return operationalError(err)
		}
	}

	faults, err := fixture.BuildFaults(opts.seed, clean)
	if err != nil {
		return operationalError(err)
	}
	if err := faults.WriteFaults(opts.dir); err != nil {
		return operationalError(err)
	}
	checks, err := demo.VerifyFaults(faults)
	if err != nil {
		return pipelineError(err)
	}

	built, err := pipeline.Build(opts.seed, specs, clean[:])
	if err != nil {
		return pipelineError(err)
	}
	if len(built.Alignment.Segments) != fixture.DemoSegments || len(specs) != 2 {
		return pipelineError(mediaerr.New(mediaerr.KindUnrecoverable, -1, 0, false,
			"clean alignment produced segments=%d renditions=%d", len(built.Alignment.Segments), len(specs)))
	}
	if built.Alignment.MaxSkewMS > 20 {
		return pipelineError(mediaerr.New(mediaerr.KindNoCommonKeyframe, -1, 0, false,
			"maximum boundary skew %.3fms exceeds 20ms", built.Alignment.MaxSkewMS))
	}

	boundaryIndex, safePTS, err := findSafeBoundary(built, opts.interruptAfter)
	if err != nil {
		return pipelineError(err)
	}
	targetOffset := int64(opts.interruptAfter) * ingest.PacketSize
	prefixOffset := targetOffset + ingest.PacketSize
	cp := resume.New(opts.seed, opts.dir, opts.interruptAfter)
	for i := range specs {
		if err := cp.RecordInputPrefix(specs[i].ID, sourcePaths[i], prefixOffset, targetOffset,
			opts.interruptAfter+1, safePTS); err != nil {
			return pipelineError(err)
		}
	}
	checkpointPath := filepath.Join(opts.dir, "checkpoint.json")
	partial, err := pipeline.Publish(opts.dir, specs, built, cp, boundaryIndex, false)
	if err != nil {
		return pipelineError(err)
	}
	if err := cp.Save(checkpointPath); err != nil {
		return operationalError(err)
	}

	if !opts.resume {
		fmt.Printf("interrupted after packet_index=%d safe_boundary=%d partial_committed=%d\n",
			opts.interruptAfter, boundaryIndex, partial.Committed)
		fmt.Println("rerun with --resume to verify prefixes and complete atomically")
		return 0
	}

	started := time.Now()
	loaded, err := resume.Load(checkpointPath)
	if err != nil {
		return pipelineError(err)
	}
	if loaded == nil {
		return pipelineError(mediaerr.New(mediaerr.KindInvalidArgument, -1, 0, false, "checkpoint missing after interrupt"))
	}
	if err := loaded.VerifyPrefixes(); err != nil {
		return pipelineError(err)
	}
	if err := loaded.VerifyOutputs(opts.dir); err != nil {
		return pipelineError(err)
	}
	full, err := pipeline.Publish(opts.dir, specs, built, loaded, -1, true)
	if err != nil {
		return pipelineError(err)
	}
	resumeMS := time.Since(started).Milliseconds()
	safePacket := int(targetOffset / ingest.PacketSize)
	replayedPackets := opts.interruptAfter - safePacket + 1
	if replayedPackets < 0 {
		replayedPackets = 0
	}

	masterPath := filepath.Join(opts.dir, "master.m3u8")
	hash, err := verify.SHA256File(masterPath)
	if err != nil {
		return operationalError(err)
	}
	if hash != built.Master.SHA256() || len(hash) != 64 {
		return pipelineError(mediaerr.New(mediaerr.KindOutputMismatch, -1, 0, false, "master hash mismatch"))
	}
	second, err := resume.Load(checkpointPath)
	if err != nil {
		return pipelineError(err)
	}
	repeat, err := pipeline.Publish(opts.dir, specs, built, second, -1, true)
	if err != nil {
		return pipelineError(err)
	}
	hash2, err := verify.SHA256File(masterPath)
	if err != nil {
		return operationalError(err)
	}
	repeatEqual := hash == hash2

	if resumeMS > 1000 || replayedPackets > 1 {
		return pipelineError(mediaerr.New(mediaerr.KindUnrecoverable, -1, 0, false,
			"resume budget failed: resume_ms=%d replayed_packets=%d", resumeMS, replayedPackets))
	}

	fmt.Printf("clean segments=%d renditions=%d\n", len(built.Alignment.Segments), len(specs))
	fmt.Printf("skew_ms=%.3f bound_ms=20\n", built.Alignment.MaxSkewMS)
	fmt.Println("skew_ms<=20")
	fmt.Printf("damage first_bad_offset=%d offset_error_packets=%d\n", checks.FirstBadOffset, checks.OffsetErrorPackets)
	fmt.Printf("faults continuity_gaps=%d retained_psi_version=%d rtp_duplicates=%d\n",
		checks.ContinuityGaps, checks.RetainedPSIVersion, checks.RTPDuplicates)
	fmt.Printf("resume_ms=%d replayed_packets=%d committed_outputs=%d replayed_outputs=%d repeat_replayed_outputs=%d\n",
		resumeMS, replayedPackets, full.Committed, full.Replayed, repeat.Replayed)
	fmt.Println("resume_ms<=1000 replayed_packets<=1")
	fmt.Printf("output_sha256=%s repeat_equal=%v\n", hash, repeatEqual)
	if !repeatEqual {
		return 1
	}
	return 0
}

func findSafeBoundary(built *pipeline.Built, packetIndex int) (int, int64, error) {
	target := int64(packetIndex) * ingest.PacketSize
	for bi, b := range built.Alignment.Boundaries {
		matched := true
		for r := range built.Tracks {
			pts := b.RenditionPTS[r]
			off := int64(-1)
			for _, f := range built.Tracks[r].Frames {
				if f.Keyframe && f.PTS == pts {
					off = f.Offset
					break
				}
			}
			if off != target {
				matched = false
				break
			}
		}
		if matched {
			return bi, b.PTS, nil
		}
	}
	return 0, 0, mediaerr.New(mediaerr.KindNoCommonKeyframe, target, 0, false,
		"packet index %d is not a common IDR safe cut", packetIndex)
}

func usageError(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "usage error: "+format+"\n", args...)
	fmt.Fprintln(os.Stderr, "usage: mediad demo --seed N --dir DIR --faults all --interrupt-after 128 [--resume]")
	return 1
}

func operationalError(err error) int {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return 1
}

func pipelineError(err error) int {
	fmt.Fprintf(os.Stderr, "pipeline error: %v\n", err)
	return mediaerr.ExitCode(err)
}
