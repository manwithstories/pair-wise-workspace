package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/relayteam/relay/internal/app"
	"github.com/relayteam/relay/internal/fixture"
	"github.com/relayteam/relay/internal/media"
)

func runDemo(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := bindCommon(fs)
	segments := fs.Int("segments", 1200, "number of deterministic NDJSON timeline records")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return media.ExitOK
		}
		fmt.Printf("relay_result path=demo status=arg_error error=%q\n", err.Error())
		return media.ExitUsage
	}
	if err := c.validate(); err != nil {
		fmt.Printf("relay_result path=demo status=arg_error error=%q\n", err.Error())
		return media.ExitUsage
	}
	if *segments < fixture.StreamCount || *segments%fixture.StreamCount != 0 {
		fmt.Printf("relay_result path=demo status=arg_error error=%q\n",
			"--segments must be positive and divisible by track*rendition count")
		return media.ExitUsage
	}
	if c.interruptAfter == 0 {
		c.interruptAfter = 5
	}

	set, err := fixture.GenerateAll(fixture.Options{Root: c.root, Records: *segments, PayloadBytes: 4096})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fixture generation failed: %v\n", err)
		return 1
	}
	fmt.Printf("demo_start root=%s records=%d chunk_records=%d max_inflight_bytes=%d slow_every=%d interrupt_after=%d\n",
		c.root, *segments, c.chunkRecords, c.maxInflightBytes, c.slowEvery, c.interruptAfter)

	normalCfg := app.RunConfig{
		Root: filepath.Join(c.root, "normal"), InputPath: set.NormalPath,
		ChunkRecords: c.chunkRecords, MaxInflightBytes: c.maxInflightBytes,
		SlowEvery: c.slowEvery, SlowDelay: 2 * time.Millisecond,
	}
	normal, err := app.Run(ctx, normalCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "normal path failed: %v\n", err)
		return media.ExitCode(err)
	}
	printRunLine("normal", "pass", normal, normal.RestoreMS, true, normal.SHA256)

	corruptOK, crcBad, transitionBad := runCorruptDemo(c.root, *segments, set)

	recoveryRoot := filepath.Join(c.root, "recovery")
	firstCfg := normalCfg
	firstCfg.Root = recoveryRoot
	firstCfg.InterruptAfterChunks = c.interruptAfter
	first, firstErr := app.Run(ctx, firstCfg)
	interruptCode := media.ExitCode(firstErr)
	if !errors.Is(firstErr, media.ErrInterrupted) {
		fmt.Fprintf(os.Stderr, "expected recoverable interrupt, got: %v\n", firstErr)
		return 1
	}
	if first == nil {
		first = &app.RunResult{}
	}
	fmt.Printf("relay_result path=recovery_interrupt status=interrupted exit_code=%d records=%d chunks=%d first_bad_record=0 precision_ticks=0 backpressure_peak_bytes=%d restore_ms=%d recover_match=false sha256=%s\n",
		interruptCode, first.Records, first.Chunks, first.BackpressurePeakBytes, first.RestoreMS, strings.Repeat("0", 64))

	resumeCfg := firstCfg
	resumeCfg.InterruptAfterChunks = 0
	recovery, err := app.Run(ctx, resumeCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recovery path failed: %v\n", err)
		return media.ExitCode(err)
	}
	idempotent, err := app.Run(ctx, resumeCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "idempotent rerun failed: %v\n", err)
		return media.ExitCode(err)
	}
	recoverMatch := recovery.SHA256 == normal.SHA256 && idempotent.SHA256 == normal.SHA256
	printRunLine("recovery", "pass", recovery, recovery.RestoreMS, recoverMatch, recovery.SHA256)
	fmt.Printf("relay_result path=idempotent_rerun status=pass records=%d first_bad_record=0 precision_ticks=%d backpressure_peak_bytes=%d restore_ms=%d recover_match=%t sha256=%s\n",
		idempotent.Records, idempotent.PrecisionTicks, idempotent.BackpressurePeakBytes,
		idempotent.RestoreMS, idempotent.SHA256 == normal.SHA256, idempotent.SHA256)

	peak := recovery.BackpressurePeakBytes
	if first.BackpressurePeakBytes > peak {
		peak = first.BackpressurePeakBytes
	}
	if !corruptOK {
		fmt.Printf("DEMO_FAIL reason=corruption_not_located exit_code=3 records=%d first_bad_record=%d sha256=%s\n",
			*segments, crcBad, normal.SHA256)
		return media.ExitCorruptUnlocated
	}
	if !recoverMatch || recovery.PrecisionTicks != 0 || peak > c.maxInflightBytes || recovery.RestoreMS >= 5000 {
		fmt.Printf("DEMO_FAIL recover_match=%t precision_ticks=%d peak=%d limit=%d restore_ms=%d\n",
			recoverMatch, recovery.PrecisionTicks, peak, c.maxInflightBytes, recovery.RestoreMS)
		return 1
	}
	fmt.Printf("DEMO_PASS path=all records=%d first_bad_record=%d crc_first_bad_record=%d transition_first_bad_record=%d precision_ticks=%d backpressure_peak_bytes=%d restore_ms=%d recover_match=true sha256=%s normal_sha256=%s recovery_sha256=%s idempotent_sha256=%s interrupt_exit_code=%d\n",
		*segments, crcBad, crcBad, transitionBad, recovery.PrecisionTicks, peak,
		recovery.RestoreMS, normal.SHA256, normal.SHA256, recovery.SHA256, idempotent.SHA256, interruptCode)
	return media.ExitOK
}

func printRunLine(path, status string, r *app.RunResult, restoreMS int64, match bool, sha string) {
	fmt.Printf("relay_result path=%s status=%s records=%d first_bad_record=%d precision_ticks=%d backpressure_peak_bytes=%d restore_ms=%d recover_match=%t sha256=%s\n",
		path, status, r.Records, r.FirstBadRecord, r.PrecisionTicks,
		r.BackpressurePeakBytes, restoreMS, match, sha)
}

func runCorruptDemo(root string, records int, set *fixture.Set) (bool, int, int) {
	allOK := true
	crcBad, transitionBad := 0, 0
	for _, exp := range set.Expectations {
		d := app.DiagnoseFile(exp.Path, filepath.Join(root, "manifest-errors", exp.Case))
		stageText := strings.Join(d.Stages, ">")
		stageOK := true
		if exp.WantStages >= 3 {
			stageOK = len(d.Stages) >= 3 &&
				strings.Contains(stageText, "segment.cut") &&
				strings.Contains(stageText, "manifest.hls")
		}
		ok := d.RecordNo == exp.RecordNo && d.Code == exp.WantCode && d.SHA256 != "" && stageOK
		if exp.Case == "crc32" {
			crcBad = d.RecordNo
		}
		if exp.Case == "transition" {
			transitionBad = d.RecordNo
		}
		status := "located"
		if !ok {
			status = "location_mismatch"
			allOK = false
		}
		fmt.Printf("relay_result path=corrupt case=%s status=%s records=%d first_bad_record=%d expected_bad_record=%d precision_ticks=0 backpressure_peak_bytes=0 restore_ms=0 recover_match=true located=%t stage_match=%t code=%s sha256=%s stages=%s\n",
			exp.Case, status, records, d.RecordNo, exp.RecordNo, d.Located, stageOK,
			d.Code, emptyDigest(d.SHA256), stageText)
		if !ok && d.Err != nil {
			fmt.Fprintf(os.Stderr, "corrupt diagnosis mismatch: %v\n", d.Err)
		}
	}
	return allOK, crcBad, transitionBad
}
