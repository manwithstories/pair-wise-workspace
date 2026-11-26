package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/archive/tsrestore/internal/sample"
)

// printReport emits the required normal/damage/resume terminal summaries and,
// when --hashes is set, every segment id with its 64-character sha256.
func printReport(v *verifyFlags, normal *normalSummary, damage *phaseMetrics, resume *resumeSummary) {
	fmt.Println("================ archive restore report ================")
	fmt.Printf("seed=%d loss=%.3f reorder=%d slow-consumer=%s out=%s\n",
		v.seed, v.loss, v.reorder, v.slowConsumer, v.out)

	fmt.Println("--- normal ---")
	printMetrics(normal.metrics)

	fmt.Println("--- damage ---")
	fmt.Printf("isolated=%d recovered=%s\n", damage.isolated, damage.recovered)
	printMetrics(*damage)

	if resume != nil {
		fmt.Println("--- resume ---")
		fmt.Printf("cache_reject=%d cache_hit=%d\n",
			resume.metrics.cacheReject, resume.metrics.cacheHit)
		printMetrics(resume.metrics)
	}

	if v.hashes {
		fmt.Println("--- segment hashes ---")
		printHashes(normal.hashes)
		if resume != nil {
			fmt.Println("--- resume segment hashes ---")
			printHashes(resume.hashes)
		}
	}
}

func printMetrics(m phaseMetrics) {
	fmt.Printf("roundtrip=%.2f%% keyframe_hit=%.2f%% aligned=%.2f%% hash_match=%.2f%%\n",
		m.roundtripPct, m.keyframeHitPct, m.alignedPct, m.hashMatchPct)
	fmt.Printf("segments=%d discontinuities=%d peak_buffer_bytes=%d\n",
		m.segments, m.disc, m.peakBuffer)
}

func printHashes(hashes map[string]string) {
	ids := make([]string, 0, len(hashes))
	for id := range hashes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Printf("segment_id=%s sha256=%s\n", id, hashes[id])
	}
}

// assertAll enforces the acceptance contract. It returns exitOK only when every
// required figure meets its target; the resume phase additionally demands
// exactly one cache rejection and a full hash match.
func assertAll(v *verifyFlags, normal *normalSummary, damage *phaseMetrics,
	resume *resumeSummary) int {
	const expectedTotal = 120 // 2 renditions x 60 six-second segments at 360 s
	fail := func(msg string) int {
		fmt.Fprintln(os.Stderr, "ASSERT FAIL:", msg)
		return exitFatal
	}
	if normal.metrics.segments != expectedTotal {
		return fail(fmt.Sprintf("segments=%d want %d", normal.metrics.segments, expectedTotal))
	}
	if damage.segments != expectedTotal {
		return fail(fmt.Sprintf("damage segments=%d want %d", damage.segments, expectedTotal))
	}
	for name, m := range map[string]phaseMetrics{"normal": normal.metrics, "damage": *damage} {
		if m.roundtripPct != 100.0 {
			return fail(name + " roundtrip not 100%")
		}
		if m.keyframeHitPct != 100.0 {
			return fail(name + " keyframe_hit not 100%")
		}
		if m.alignedPct != 100.0 {
			return fail(name + " aligned not 100%")
		}
		if m.peakBuffer > pipelineCap {
			return fail(name + " peak buffer over cap")
		}
	}
	if damage.recovered != "ok" {
		return fail("damage recovery not ok")
	}
	if damage.isolated == 0 {
		return fail("expected isolated gap events under loss")
	}
	if resume != nil {
		if resume.metrics.cacheReject != 1 {
			return fail(fmt.Sprintf("cache_reject=%d want 1", resume.metrics.cacheReject))
		}
		if resume.metrics.hashMatchPct != 100.0 {
			return fail("resume hash_match not 100%")
		}
		if !hashesEqual(normal.hashes, resume.hashes) {
			return fail("resume hashes differ from initial run")
		}
	}
	fmt.Println("ALL ASSERTIONS PASSED")
	return exitOK
}

const pipelineCap = 16 * 1024 * 1024

func hashesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// runFixture materializes one of the edge-case fixtures to --out so the
// isolation/recovery/rejection paths can be exercised standalone.
func runFixture(args []string) int {
	fs := flag.NewFlagSet("fixture", flag.ContinueOnError)
	out := fs.String("out", "", "output directory")
	seed := fs.Int64("seed", 41, "fixture seed")
	if err := fs.Parse(args); err != nil {
		return exitFatal
	}
	if fs.NArg() < 1 || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: restore fixture <kind> --out DIR")
		return exitFatal
	}
	kind := fs.Arg(0)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitFatal
	}
	r := sample.DefaultRenditions()[0]
	cfg := sample.DamageConfig{Loss: 0.02, ReorderDist: 9, Duplicates: 0.02}
	stream, _, err := sample.BuildRendition(r, sample.ProgramDuration(24), *seed, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitFatal
	}
	switch kind {
	case "truncated":
		stream = append(stream, 0x0F, 0xA0) // length prefix promising more than present
	case "bad-rtcp", "tamper-cache", "slow-consumer":
		// These kinds are driven through `verify`; the fixture command writes
		// the base damaged stream for offline inspection.
	default:
		fmt.Fprintln(os.Stderr, "unknown fixture kind:", kind)
		return exitFatal
	}
	path := filepath.Join(*out, "fixture-"+kind+".bin")
	if err := os.WriteFile(path, stream, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitFatal
	}
	fmt.Println("wrote", path, "bytes=", len(stream))
	return exitOK
}
