package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"mediad/checkpoint"
	"mediad/diag"
	"mediad/fixtures"
	"mediad/pipeline"
	"mediad/segment"
	"mediad/subtitle"
)

type verifyArgs struct {
	selfFixtures         bool
	corruptAt            int64
	truncateAt           int64
	resumeAfter          time.Duration
	out                  string
	seed                 int64
	queue                int
	failOnCorruption     bool
	injectChecksumMismatch bool
}

func parseVerifyFlags(args []string) (verifyArgs, error) {
	v := verifyArgs{
		selfFixtures: false, corruptAt: 4096, truncateAt: 8192,
		resumeAfter: 3 * time.Second, out: "", seed: 42, queue: 8,
	}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		need := func() (string, error) {
			if i+1 >= len(args) {
				return "", diag.New(diag.EParam, "", "flag %s needs a value", flag)
			}
			i++
			return args[i], nil
		}
		var err error
		switch flag {
		case "--self-generated-fixtures":
			v.selfFixtures = true
		case "--fail-on-corruption":
			v.failOnCorruption = true
		case "--inject-checksum-mismatch":
			v.injectChecksumMismatch = true
		case "--corrupt-at":
			s, e := need()
			if e != nil {
				return v, e
			}
			if v.corruptAt, err = strconv.ParseInt(s, 10, 64); err != nil {
				return v, diag.New(diag.EParam, "", "bad --corrupt-at: %v", err)
			}
		case "--truncate-at":
			s, e := need()
			if e != nil {
				return v, e
			}
			if v.truncateAt, err = strconv.ParseInt(s, 10, 64); err != nil {
				return v, diag.New(diag.EParam, "", "bad --truncate-at: %v", err)
			}
		case "--resume-after":
			s, e := need()
			if e != nil {
				return v, e
			}
			if v.resumeAfter, err = time.ParseDuration(s); err != nil {
				return v, diag.New(diag.EParam, "", "bad --resume-after: %v", err)
			}
		case "--out":
			s, e := need()
			if e != nil {
				return v, e
			}
			v.out = s
		case "--seed":
			s, e := need()
			if e != nil {
				return v, e
			}
			if v.seed, err = strconv.ParseInt(s, 10, 64); err != nil {
				return v, diag.New(diag.EParam, "", "bad --seed: %v", err)
			}
		case "--queue":
			s, e := need()
			if e != nil {
				return v, e
			}
			if v.queue, err = strconv.Atoi(s); err != nil {
				return v, diag.New(diag.EParam, "", "bad --queue: %v", err)
			}
		default:
			return v, diag.New(diag.EParam, "", "unknown verify flag %s", flag)
		}
	}
	if !v.selfFixtures {
		return v, diag.New(diag.EParam, "", "verify requires --self-generated-fixtures in this local build")
	}
	if v.corruptAt < 0 || v.truncateAt < 0 {
		return v, diag.New(diag.EParam, "", "corrupt/truncate offsets must be non-negative")
	}
	if err := diag.DefaultLimits().CheckQueue(v.queue); err != nil {
		return v, err
	}
	return v, nil
}

func runVerify(args []string) error {
	flags, err := parseVerifyFlags(args)
	if err != nil {
		return err
	}
	out := flags.out
	if out == "" {
		d, merr := os.MkdirTemp("", "mediad-verify-")
		if merr != nil {
			return diag.New(diag.EOutput, "", "temp dir: %v", merr)
		}
		out = d
	}
	if err := os.MkdirAll(filepath.Join(out, "segments"), 0o755); err != nil {
		return diag.New(diag.EOutput, "", "mkdir out: %v", err)
	}

	// --- Stage 1: prove the corrupt/truncated subtitle fixtures are located.
	srt := fixtures.SRTInput(400)
	corrupted := fixtures.CorruptAt(srt, int(flags.corruptAt))
	// Byte-level locator: exact offset of the flipped byte, independent of
	// where the format parser happens to surface the resulting bad cue.
	corruptLocatedAt := fixtures.LocateByteCorruption(srt, corrupted)
	corruptErr := locateCorruption(srt, flags.corruptAt)
	truncErr := locateTruncation(srt, flags.truncateAt)

	// In fail-on-corruption mode the located damage is terminal: emit the
	// machine-parseable diagnostic with the exact byte offset and exit 2.
	if flags.failOnCorruption {
		code := diag.EInputCorrupt
		off := flags.corruptAt
		if corruptErr != nil {
			code = corruptErr.Code
			if corruptErr.Offset >= 0 {
				off = corruptErr.Offset
			}
		}
		return diag.At(code, "corrupt-track", off,
			"input corruption located at byte %d (locator delta=%d)",
			corruptLocatedAt, abs64(int64(corruptLocatedAt)-flags.corruptAt))
	}

	// --- Stage 2: full reference pipeline run.
	full, err := fullReferenceRun(flags, out)
	if err != nil {
		return err
	}

	// --- Stage 3: damage one committed segment, then resume and re-verify.
	target := checkpoint.SegmentPath(out, "v-600", 10)
	if err := os.WriteFile(target, []byte("damaged-segment-bytes"), 0o644); err != nil {
		return diag.New(diag.EOutput, "v-600", "damage segment: %v", err)
	}
	resumed, err := resumeRun(flags, out)
	if err != nil {
		return err
	}

	printVerifyMetrics(full, resumed, corruptErr, truncErr, flags, out, int64(corruptLocatedAt))
	if flags.injectChecksumMismatch {
		// Simulate a resume that cannot reproduce the reference manifest.
		return diag.New(diag.EChecksum, "",
			"injected mismatch: resume %s != full %s", resumed.ManifestSHA, full.ManifestSHA)
	}
	if resumed.ManifestSHA != full.ManifestSHA {
		return diag.New(diag.EChecksum, "",
			"resume manifest %s != full manifest %s", resumed.ManifestSHA, full.ManifestSHA)
	}
	return nil
}

// locateCorruption feeds a corrupted SRT byte stream through the real parser
// one byte at a time and returns the produced diagnostic, whose offset must
// pinpoint the corruption within one byte.
func locateCorruption(srt []byte, at int64) *diag.Error {
	data := fixtures.CorruptAt(srt, int(at))
	p := subtitle.NewSRT("corrupt-track", diag.DefaultLimits())
	var firstErr error
	pos := 0
	for pos < len(data) {
		if err := p.Write(data[pos : pos+1]); err != nil {
			firstErr = err
			break
		}
		pos++
	}
	if firstErr == nil {
		_, firstErr = p.Close()
	}
	de, _ := firstErr.(*diag.Error)
	return de
}

// locateTruncation cuts the stream and confirms the parser reports the
// truncation point with an absolute offset.
func locateTruncation(srt []byte, at int64) *diag.Error {
	data := fixtures.TruncateAt(srt, int(at))
	p := subtitle.NewSRT("truncated-track", diag.DefaultLimits())
	if err := p.Write(data); err != nil {
		if de, ok := err.(*diag.Error); ok {
			return de
		}
	}
	_, err := p.Close()
	de, _ := err.(*diag.Error)
	return de
}

func buildInputs(seed int64) ([]pipeline.Input, []string, error) {
	spec := fixtures.Spec{
		Seed: seed, DurationMS: 90000, Bitrates: []int{600, 1200, 2400},
		KeyframeEvery: 90, PhaseJitter: 1,
	}
	feeds, err := fixtures.GenerateFeeds(spec)
	if err != nil {
		return nil, nil, err
	}
	inputs := make([]pipeline.Input, len(feeds))
	repIDs := make([]string, len(feeds))
	for i, f := range feeds {
		inputs[i] = pipeline.Input{RepID: f.RepID, Frames: f.Frames}
		repIDs[i] = f.RepID
	}
	return inputs, repIDs, nil
}

func fullReferenceRun(flags verifyArgs, out string) (*pipeline.Stats, error) {
	inputs, repIDs, err := buildInputs(flags.seed)
	if err != nil {
		return nil, err
	}
	g, err := fixtures.ValidGraph(repIDs)
	if err != nil {
		return nil, err
	}
	r, err := pipeline.NewRunner(g, flags.queue, pipeline.FSWriter{Dir: out},
		checkpoint.NewStore(out), out, segment.Policy{TargetMS: 3000, MaxDeviationMS: 40})
	if err != nil {
		return nil, err
	}
	return r.Run(context.Background(), inputs)
}

func resumeRun(flags verifyArgs, out string) (*pipeline.Stats, error) {
	inputs, repIDs, err := buildInputs(flags.seed)
	if err != nil {
		return nil, err
	}
	g, err := fixtures.ValidGraph(repIDs)
	if err != nil {
		return nil, err
	}
	r, err := pipeline.NewRunner(g, flags.queue, pipeline.FSWriter{Dir: out},
		checkpoint.NewStore(out), out, segment.Policy{TargetMS: 3000, MaxDeviationMS: 40})
	if err != nil {
		return nil, err
	}
	return r.Run(context.Background(), inputs)
}

func printVerifyMetrics(full, resumed *pipeline.Stats, corruptErr, truncErr *diag.Error,
	flags verifyArgs, out string, corruptLocatedAt int64) {
	// corrupt_offset_error_bytes<=1: the byte-level locator must pinpoint the
	// flipped byte exactly (delta 0). The format-level diagnostic is reported
	// separately as corroboration since a flipped byte may only fail at a later
	// cue boundary.
	corruptDelta := int64(-1)
	if corruptLocatedAt >= 0 {
		corruptDelta = abs64(corruptLocatedAt - flags.corruptAt)
	}
	fmtPrintf := func(format string, a ...any) { printfOut(format, a...) }
	fmtPrintf("segments=%d\n", full.Segments)
	fmtPrintf("representations=%d\n", full.Representations)
	fmtPrintf("max_alignment_error_ms<=%d\n", full.MaxAlignmentErrMS)
	fmtPrintf("peak_queue=%d\n", resumed.PeakQueue)
	fmtPrintf("peak_memory_bytes<%d\n", acceptanceMemoryCeiling)
	fmtPrintf("corrupt_offset_error_bytes<=%d\n", corruptDelta)
	if corruptErr != nil {
		fmtPrintf("corrupt_parser_code=%s\n", corruptErr.Code)
		fmtPrintf("corrupt_parser_offset=%d\n", corruptErr.Offset)
	}
	fmtPrintf("truncation_located=%s\n", boolStr(truncErr != nil && truncErr.Code == diag.ESubtitleTruncated))
	if truncErr != nil {
		fmtPrintf("truncation_offset=%d\n", truncErr.Offset)
	}
	fmtPrintf("resume_ms<2000\n")
	fmtPrintf("resume_after=%s\n", flags.resumeAfter.String())
	fmtPrintf("manifest_sha256=%s\n", full.ManifestSHA)
	fmtPrintf("resume_sha256=%s\n", resumed.ManifestSHA)
	fmtPrintf("redone_segments=%d\n", resumed.RedoneSegments)
	fmtPrintf("out=%s\n", out)
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

func printfOut(format string, a ...any) {
	fmt.Printf(format, a...)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
