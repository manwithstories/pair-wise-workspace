// Command ingestcheck drives the media convergence library over locally
// generated samples and prints the acceptance metrics. Exit code 0 means the
// metrics met target, 1 means verification failed, 2 means bad arguments or
// usage. Output files are written under TMPDIR; no build artifacts are kept
// in the repository.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pairwise/ingestcheck/internal/clock"
	"pairwise/ingestcheck/internal/fixture"
	"pairwise/ingestcheck/internal/pipeline"
)

type outcomes struct {
	pair     *pipeline.Report
	damaged  *pipeline.Report
	recovery *pipeline.Report
	clean    *pipeline.Report
}

type exitErr struct {
	code int
	msg  string
}

func (e *exitErr) Error() string { return e.msg }

func main() {
	seed := flag.Int64("seed", 41, "deterministic sample seed")
	mode := flag.String("mode", "all", "sample set: all, pair, damaged, recovery, clean")
	out := flag.String("out", "", "output directory for digests (defaults under TMPDIR)")
	flag.Parse()

	res, code := run(*seed, *mode, *out)
	os.Exit(code)
	_ = res
}

func run(seed int64, mode, out string) (outcomes, int) {
	var o outcomes
	switch mode {
	case "all", "pair", "damaged", "recovery", "clean":
	default:
		fmt.Fprintf(os.Stderr, "ingestcheck: unknown mode %q\n", mode)
		return o, 2
	}
	if seed == 0 {
		fmt.Fprintln(os.Stderr, "ingestcheck: seed must be non-zero")
		return o, 2
	}
	if out == "" {
		out = filepath.Join(os.Getenv("TMPDIR"), "pair-flow-demo")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "ingestcheck: %v\n", err)
		return o, 2
	}

	runner, err := pipeline.New(pipeline.DefaultConfig())
	if err != nil {
		fmt.Fprintf(os.Stderr, "ingestcheck: %v\n", err)
		return o, 2
	}
	wantPair := mode == "all" || mode == "pair" || mode == "clean"
	wantDam := mode == "all" || mode == "damaged"
	wantRec := mode == "all" || mode == "recovery"

	failed := false
	fmt.Printf("seed=%d mode=%s out=%s\n", seed, mode, out)

	if wantPair {
		pair, err := runner.Run(fixture.Pair())
		if err != nil {
			fmt.Fprintf(os.Stderr, "ingestcheck: pair: %v\n", err)
			return o, 1
		}
		o.pair = pair
		printPair("pair", pair)
		if !pairRoundTripPerfect(pair) {
			failed = true
		}
		// A 200 ms shift must be reported as unsynchronised, not corrected.
		desync, err := runner.Run(fixture.Desync())
		if err != nil {
			fmt.Fprintf(os.Stderr, "ingestcheck: desync: %v\n", err)
			return o, 1
		}
		fmt.Printf("desync unsynchronized=%d av_skew_p95_ms=%.2f\n", desync.Unsynchronized, desync.SkewP95Ms)
		if desync.Unsynchronized == 0 {
			failed = true
		}
	}
	if wantDam {
		dam, err := runner.Run(fixture.Truncated())
		if err != nil {
			fmt.Fprintf(os.Stderr, "ingestcheck: damaged: %v\n", err)
			return o, 1
		}
		o.damaged = dam
		printDamaged(dam)
		if dam.Isolated == 0 || dam.FirstBadByte < 0 {
			failed = true
		}
	}
	if wantRec {
		rec, err := runner.Run(fixture.Burst())
		if err != nil {
			fmt.Fprintf(os.Stderr, "ingestcheck: recovery: %v\n", err)
			return o, 1
		}
		o.recovery = rec
		printRecovery(rec)
		if rec.DuplicateDrop == 0 || (rec.Recovered+rec.ExplicitGap) == 0 {
			failed = true
		}
		// Repeated runs must hash identically.
		again, err := runner.Run(fixture.Burst())
		if err != nil {
			fmt.Fprintf(os.Stderr, "ingestcheck: recovery-rerun: %v\n", err)
			return o, 1
		}
		stable := rec.AuditRoot == again.AuditRoot && len(rec.SegmentHashes) == len(again.SegmentHashes)
		fmt.Printf("repeat_hash=%s\n", boolWord(stable))
		if !stable {
			failed = true
		}
		// Wrap sample drives duplicate and late accounting.
		wrap, err := runner.Run(fixture.Wrap())
		if err != nil {
			fmt.Fprintf(os.Stderr, "ingestcheck: wrap: %v\n", err)
			return o, 1
		}
		printWrap(wrap)
		// Timeline regression opens a new epoch while keeping raw ticks.
		reg, err := runner.Run(fixture.Regress())
		if err != nil {
			fmt.Fprintf(os.Stderr, "ingestcheck: regress: %v\n", err)
			return o, 1
		}
		epochs := 0
		for _, t := range reg.Tracks {
			epochs += t.Epochs
		}
		fmt.Printf("regress epochs=%d segment_sha256=%d/%d\n", epochs, reg.SegmentSHAOK, reg.SegmentSHATotal)
		if epochs == 0 {
			failed = true
		}
	}
	if wantPair {
		// Clean re-run proves determinism.
		clean, err := runner.Run(fixture.Pair())
		if err != nil {
			fmt.Fprintf(os.Stderr, "ingestcheck: clean: %v\n", err)
			return o, 1
		}
		o.clean = clean
		stable := clean.AuditRoot == o.pair.AuditRoot
		fmt.Printf("clean_repeat_hash=%s audit_root=%s\n", boolWord(stable), clean.AuditRoot)
		if !stable {
			failed = true
		}
	}

	writeReport(out, o)
	if failed {
		fmt.Println("result=FAIL")
		return o, 1
	}
	fmt.Println("result=PASS")
	return o, 0
}

func pairRoundTripPerfect(r *pipeline.Report) bool {
	if r.RoundTripTotal == 0 {
		return false
	}
	return r.RoundTripOK == r.RoundTripTotal && r.SegmentSHAOK == r.SegmentSHATotal && r.KeyframeHits == r.KeyframeTotal
}

func printPair(label string, r *pipeline.Report) {
	fmt.Printf("roundtrip=%d/%d keyframe_hit=%d/%d segment_sha256=%d/%d\n",
		r.RoundTripOK, r.RoundTripTotal, r.KeyframeHits, r.KeyframeTotal, r.SegmentSHAOK, r.SegmentSHATotal)
	fmt.Printf("av_skew_p95_ms=%.2f av_skew_max_ms=%.2f evaluated=%d unsynchronized=%d\n",
		r.SkewP95Ms, r.SkewMaxMs, r.Evaluated, r.Unsynchronized)
	for _, h := range r.SegmentHashes {
		fmt.Printf("segment track=%d seq=%d bytes=%d sha256=%s\n", h.TrackID, h.Seq, h.Bytes, h.Sha256)
	}
}

func printDamaged(r *pipeline.Report) {
	fmt.Printf("damaged isolated=%d first_bad_byte=%d\n", r.Isolated, r.FirstBadByte)
	if len(r.IsolationCodes) > 0 {
		fmt.Printf("damaged codes=%s\n", strings.Join(r.IsolationCodes, ","))
	}
}

func printRecovery(r *pipeline.Report) {
	fmt.Printf("recovery duplicate_drop=%d recovered=%d explicit_gap=%d av_skew_p95_ms=%.2f\n",
		r.DuplicateDrop, r.Recovered, r.ExplicitGap, r.SkewP95Ms)
	for _, t := range r.Tracks {
		fmt.Printf("track=%d parsed=%d isolated=%d epochs=%d header_gaps=%d packet_gaps=%d recovered=%d explicit=%d keyframes=%d\n",
			t.TrackID, t.Parsed, t.Isolated, t.Epochs, t.HeaderGaps, t.PacketGaps, t.Recovered, t.Explicit, t.Keyframes)
	}
}

func printWrap(r *pipeline.Report) {
	fmt.Printf("wrap duplicate_drop=%d late=%d lost=%d max_held=%d\n",
		r.DuplicateDrop, totalLate(r), totalLost(r), maxHeld(r))
}

func totalLate(r *pipeline.Report) int {
	n := 0
	for _, t := range r.Tracks {
		n += t.Late
	}
	return n
}

func totalLost(r *pipeline.Report) int {
	n := 0
	for _, t := range r.Tracks {
		n += t.Lost
	}
	return n
}

func maxHeld(r *pipeline.Report) int {
	n := 0
	for _, t := range r.Tracks {
		if t.MaxHeldPeak > n {
			n = t.MaxHeldPeak
		}
	}
	return n
}

func writeReport(dir string, o outcomes) {
	body := map[string]any{}
	add := func(name string, r *pipeline.Report) {
		if r == nil {
			return
		}
		body[name] = map[string]any{
			"roundtrip_ok":    r.RoundTripOK,
			"roundtrip_total": r.RoundTripTotal,
			"keyframe_hits":   r.KeyframeHits,
			"keyframe_total":  r.KeyframeTotal,
			"isolated":        r.Isolated,
			"first_bad_byte":  r.FirstBadByte,
			"duplicate_drop":  r.DuplicateDrop,
			"recovered":       r.Recovered,
			"explicit_gap":    r.ExplicitGap,
			"av_skew_p95_ms":  r.SkewP95Ms,
			"audit_root":      r.AuditRoot,
			"segments":        r.SegmentHashes,
		}
	}
	add("pair", o.pair)
	add("damaged", o.damaged)
	add("recovery", o.recovery)
	add("clean", o.clean)
	buf, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "report.json"), buf, 0o644)

	// Per-segment digest files, sorted for deterministic bytes on disk.
	var lines []string
	for _, r := range []*pipeline.Report{o.pair, o.damaged, o.recovery} {
		if r == nil {
			continue
		}
		for _, h := range r.SegmentHashes {
			lines = append(lines, fmt.Sprintf("%d %d %d %s", h.TrackID, h.Seq, h.Bytes, h.Sha256))
		}
	}
	sort.Strings(lines)
	_ = os.WriteFile(filepath.Join(dir, "segments.sha256"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func boolWord(b bool) string {
	if b {
		return "stable"
	}
	return "unstable"
}

var _ = clock.CDN
