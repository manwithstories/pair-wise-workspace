// Package pipeline wires the EBML parser, track graph, remux writer,
// manifest builder and recovery checkpoints into one deterministic flow.
package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"

	"mkvarchive/internal/ebml"
	"mkvarchive/internal/model"
	"mkvarchive/internal/recovery"
	"mkvarchive/internal/remux"
	"mkvarchive/internal/timeline"
	"mkvarchive/internal/track"
)

// Outcome classifies how a run ended and maps to the CLI exit codes.
type Outcome int

const (
	// OutcomeSuccess means every source block was archived.
	OutcomeSuccess Outcome = iota
	// OutcomePartial means damage left some confirmed blocks recoverable.
	OutcomePartial
	// OutcomeUnrecoverable means nothing could be archived safely.
	OutcomeUnrecoverable
)

// ExitCode translates an outcome to the documented process exit code.
func (o Outcome) ExitCode() int {
	switch o {
	case OutcomeSuccess:
		return 0
	case OutcomePartial:
		return 2
	default:
		return 1
	}
}

// String renders the outcome for terminal output.
func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomePartial:
		return "partial"
	default:
		return "unrecoverable"
	}
}

// Result is the complete evidence record of one pipeline run.
type Result struct {
	Outcome           Outcome
	RecoveredBlocks   int
	TotalSourceBlocks int
	FirstErrorOffset  int64 // -1 when no parse error occurred
	FirstErrorCode    string
	Quality           remux.QualityReport
	Manifest          *timeline.Manifest
	Checkpoint        recovery.CheckpointFile
	PlanJSON          []byte
	Warnings          []model.ArchiveError
}

// Options controls explicit retention policies and target bitrate.
type Options struct {
	AudioPolicy       audioPolicy
	VideoPolicy       videoPolicy
	TargetBitrateKbps int64
}

// Policy aliases keep this package free of deep import churn in callers.
type audioPolicy = int
type videoPolicy = int

// PolicyRetain is the explicit retain policy value shared by audio/video.
const PolicyRetain = 1

// DefaultOptions retains declared metadata and targets the acceptance rate.
func DefaultOptions() Options {
	return Options{
		AudioPolicy:       PolicyRetain,
		VideoPolicy:       PolicyRetain,
		TargetBitrateKbps: 12_000,
	}
}

// Archive runs the full flow against src and writes the archive into dst.
// checkpoints persist under checkpointDir. The source is streamed through a
// counting/hashing reader so peak memory stays at the block buffer size
// rather than the whole input; the returned Result holds every QA field.
func Archive(src io.Reader, dst io.Writer, checkpointDir string,
	opts Options) (*Result, error) {
	hasher := sha256.New()
	limited := io.LimitReader(src, maxStreamBytes+1)
	counter := &countingReader{r: io.TeeReader(limited, hasher)}

	scanner, err := ebml.NewScanner(counter)
	if err != nil {
		if ae, ok := err.(*model.ArchiveError); ok {
			return &Result{Outcome: OutcomeUnrecoverable,
				FirstErrorOffset: ae.Offset, FirstErrorCode: ae.Code}, err
		}
		return &Result{Outcome: OutcomeUnrecoverable, FirstErrorOffset: -1}, err
	}
	if counter.n > maxStreamBytes {
		return nil, model.NewError("INPUT_TOO_LARGE",
			"input exceeds the 64 MiB streaming memory budget", -1)
	}
	// Checkpoint identity is finalized after the whole stream has been
	// consumed; recovery.NewStore picks up any prior on-disk record.
	var store *recovery.Store
	tracks, err := track.ParseTracks(scanner.Meta().TracksData)
	if err != nil {
		return &Result{Outcome: OutcomeUnrecoverable, FirstErrorOffset: -1}, err
	}
	g, err := track.BuildGraph(tracks)
	if err != nil {
		if ae, ok := err.(*model.ArchiveError); ok {
			return &Result{Outcome: OutcomeUnrecoverable,
				FirstErrorOffset: ae.Offset, FirstErrorCode: ae.Code}, err
		}
		return &Result{Outcome: OutcomeUnrecoverable, FirstErrorOffset: -1}, err
	}
	policy := track.Policy{
		Audio: audioRetain(opts.AudioPolicy),
		Video: videoRetain(opts.VideoPolicy),
	}
	mappings, mapWarnings, err := track.BuildMapping(g, policy)
	if err != nil {
		return &Result{Outcome: OutcomeUnrecoverable, FirstErrorOffset: -1}, err
	}
	plan := remux.NewPlan(scanner.Meta().TimestampScale, mappings, mapWarnings)
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}

	builder := timeline.NewBuilder(plan.TimestampScale)
	writer, err := remux.NewWriter(dst, plan, scanner.Meta().InfoData,
		scanner.Meta().TracksData, builder)
	if err != nil {
		return nil, err
	}
	expected := map[string]bool{}
	var firstOffset int64 = -1
	var firstCode string
	recovered, confirmed, clusters := 0, 0, 0
	lastClusterTC := int64(-1)
	for {
		blk, nerr := scanner.Next()
		if nerr == io.EOF {
			break
		}
		if nerr != nil {
			if ae, ok := nerr.(*model.ArchiveError); ok {
				firstOffset, firstCode = ae.Offset, ae.Code
			}
			break
		}
		if blk.ClusterTimecode != lastClusterTC {
			clusters++
			lastClusterTC = blk.ClusterTimecode
		}
		confirmed++
		frame, ferr := remux.FrameOf(blk.Payload)
		if ferr != nil {
			if ae, ok := ferr.(*model.ArchiveError); ok {
				firstOffset, firstCode = ae.Offset, ae.Code
			}
			break
		}
		sum := sha256.Sum256(frame)
		expected[hex.EncodeToString(sum[:])] = true
		if werr := writer.WriteBlock(remux.BlockEnvelope{
			SourceTrackNumber: blk.TrackNumber,
			ElementID:         blk.ElementID,
			ClusterTimecode:   blk.ClusterTimecode,
			RawTimecode:       blk.AbsoluteTimecode(),
			HeaderOffset:      blk.ElementHeaderOff,
			Payload:           blk.Payload,
		}); werr != nil {
			return nil, werr
		}
		recovered++
	}
	// Damaged input loses at least the cluster being written when the tail
	// broke; count it so total reflects what the source intended.
	total := confirmed
	if firstOffset >= 0 {
		total = (clusters + 1) * blocksPerCluster
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	for _, w := range mapWarnings {
		builder.AddWarning(w)
	}
	manifest := builder.Build()
	// Duration spans the last observed timestamp plus one cluster step.
	var lastNS int64
	for _, tr := range manifest.Tracks {
		if tr.EndNS > lastNS {
			lastNS = tr.EndNS
		}
	}
	durationNS := lastNS + 100_000_000
	quality := writer.QualityReport(opts.TargetBitrateKbps, durationNS, expected, 0.0)

	// All source bytes (up to the damage point) have now streamed through
	// the hasher. Rebuild the store on the complete input identity so the
	// written-stage checkpoint is verifiable on the next run.
	store = recovery.NewStore(checkpointDir, "stream", counter.n, hasher.Sum(nil))
	if err = store.Mark(recovery.StageCheckpoint{Stage: recovery.StageParsed}); err != nil {
		return nil, err
	}
	if err = store.Mark(recovery.StageCheckpoint{
		Stage: recovery.StagePlanned, PlanSHA256: recovery.HashPlan(planJSON),
	}); err != nil {
		return nil, err
	}
	if err = store.Mark(recovery.StageCheckpoint{
		Stage: recovery.StageWritten, BlocksSeen: recovered,
		PayloadCount: len(expected),
	}); err != nil {
		return nil, err
	}
	outcome := OutcomeSuccess
	if firstOffset >= 0 {
		outcome = OutcomePartial
	}
	if recovered == 0 {
		outcome = OutcomeUnrecoverable
	}
	return &Result{
		Outcome:           outcome,
		RecoveredBlocks:   recovered,
		TotalSourceBlocks: total,
		FirstErrorOffset:  firstOffset,
		FirstErrorCode:    firstCode,
		Quality:           quality,
		Manifest:          manifest,
		Checkpoint:        store.File(),
		PlanJSON:          planJSON,
		Warnings:          manifest.Warnings,
	}, nil
}

// maxStreamBytes bounds a single archival input to the 64 MiB memory target.
const maxStreamBytes = 64 << 20

// blocksPerCluster is the fixed interleaving (one video + one audio block)
// produced by the generated samples.
const blocksPerCluster = 2

// countingReader tracks the total bytes consumed from a stream so the
// pipeline can establish input identity without buffering the whole input.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
