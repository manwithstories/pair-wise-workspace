package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"m3u8ctl/internal/fixture"
	"m3u8ctl/internal/playlist"
	"m3u8ctl/internal/segment"
	"m3u8ctl/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(verify.ExitUsage)
	}
	var code int
	switch os.Args[1] {
	case "fixture":
		code = runFixture(os.Args[2:])
	case "build":
		code = runBuild(os.Args[2:])
	case "verify":
		code = runVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		code = verify.ExitOK
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		code = verify.ExitUsage
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprint(os.Stderr, `m3u8ctl - local HLS playlist generator and segment verifier

usage:
  m3u8ctl fixture --out DIR --renditions 4 --segments 240 --drop 7 --shuffle --seed 42
  m3u8ctl build   --src DIR --out DIR --target-duration 4 --window 0
  m3u8ctl verify  --playlist master.m3u8 --src DIR --drift-ms 50 --out report.json

exit codes: 0 pass | 1 gaps or check failures | 2 bad arguments/path | 3 round-trip mismatch
`)
}

func runFixture(args []string) int {
	flags := newFlagSet("fixture")
	out := flags.String("out", "", "output directory for sample segments")
	renditions := flags.Int("renditions", 4, "number of renditions (1-4)")
	segments := flags.Int("segments", 240, "segments per rendition")
	drop := flags.Int("drop", 0, "number of sequence slots to drop per rendition")
	duplicates := flags.Int("dups", 3, "number of retransmitted duplicate segments per rendition")
	bad := flags.Int("bad", 0, "number of zero-byte or truncated segments per rendition")
	seed := flags.Uint64("seed", 1, "deterministic PRNG seed")
	shuffle := flags.Bool("shuffle", false, "write files in shuffled order")
	if err := flags.Parse(args); err != nil {
		return failUsage(err.Error())
	}
	if *out == "" {
		return failUsage("--out is required")
	}
	started := time.Now()
	written, err := fixture.Generate(fixture.Config{
		OutDir: *out, Renditions: *renditions, Segments: *segments,
		Drop: *drop, Duplicates: *duplicates, Bad: *bad, Shuffle: *shuffle,
		Seed: *seed, SlotMS: 4000,
	})
	if err != nil {
		return failPath(err)
	}
	fmt.Printf("fixture: wrote %d segment files to %s in %s\n",
		written, *out, time.Since(started).Round(time.Millisecond))
	return verify.ExitOK
}

func runBuild(args []string) int {
	flags := newFlagSet("build")
	src := flags.String("src", "", "source directory of segment files")
	out := flags.String("out", "", "output directory for playlists")
	target := flags.Int("target-duration", 4, "EXT-X-TARGETDURATION in seconds")
	window := flags.Int("window", 0, "sliding window size; 0 keeps all segments")
	if err := flags.Parse(args); err != nil {
		return failUsage(err.Error())
	}
	if *src == "" || *out == "" {
		return failUsage("--src and --out are required")
	}
	started := time.Now()
	scanResult, err := segment.ScanDir(*src)
	if err != nil {
		return failPath(err)
	}
	if len(scanResult.Segments) == 0 {
		return failPath(segment.ErrEmpty)
	}
	index := segment.BuildIndex(scanResult.Segments, int64(*target)*1000)
	master, medias := playlist.BuildFromIndex(index, *window)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return failPath(err)
	}
	for _, media := range medias {
		if err := playlist.WriteMedia(*out, media); err != nil {
			return failPath(err)
		}
	}
	if err := playlist.WriteMaster(*out, master); err != nil {
		return failPath(err)
	}
	var gaps int64
	for _, rIndex := range index.Ordered {
		gaps += rIndex.GapCount()
	}
	fmt.Printf("build: %d playlists in %s, %d segments, %d gap slots, %d duplicate conflicts, %s\n",
		1+len(medias), *out, len(scanResult.Segments), gaps,
		scanResult.DuplicateConflicts, time.Since(started).Round(time.Millisecond))
	return verify.ExitOK
}

func runVerify(args []string) int {
	flags := newFlagSet("verify")
	playlistPath := flags.String("playlist", "", "path to master.m3u8")
	src := flags.String("src", "", "source directory of segment files")
	driftMS := flags.Int64("drift-ms", 50, "maximum allowed EXTINF drift in milliseconds")
	out := flags.String("out", "", "write JSON report to this file; stdout when empty")
	if err := flags.Parse(args); err != nil {
		return failUsage(err.Error())
	}
	if *playlistPath == "" || *src == "" {
		return failUsage("--playlist and --src are required")
	}
	started := time.Now()
	masterParsed, err := playlist.ParseFile(*playlistPath)
	if err != nil || masterParsed.Kind != "master" {
		return failRoundTrip(fmt.Errorf("cannot parse master playlist: %w", err))
	}
	medias := make(map[int]*playlist.MediaPlaylist)
	for _, ref := range masterParsed.Master.Media {
		mediaPath := filepath.Join(filepath.Dir(*playlistPath), ref.URI)
		parsed, parseErr := playlist.ParseFile(mediaPath)
		if parseErr != nil || parsed.Kind != "media" {
			return failRoundTrip(fmt.Errorf("cannot parse media playlist %s: %w", ref.URI, parseErr))
		}
		parsed.Media.FileName = filepath.Base(mediaPath)
		medias[parsed.Media.Rendition] = parsed.Media
	}
	scanResult, err := segment.ScanDir(*src)
	if err != nil {
		return failPath(err)
	}
	targetDuration := masterParsed.Master.TargetDuration
	if targetDuration <= 0 {
		targetDuration = 4
	}
	index := segment.BuildIndex(scanResult.Segments, int64(targetDuration)*1000)
	for _, failure := range scanResult.Failures {
		fmt.Fprintf(os.Stderr, "scan failure: %s %s: %s\n", failure.Kind, failure.File, failure.Detail)
	}
	checker := verify.NewChecker(masterParsed.Master, medias, index, *driftMS)
	report := checker.Run(scanResult.DuplicateConflicts)
	for _, failure := range scanResult.Failures {
		report.Failures = append(report.Failures, verify.FailureDetail{
			Kind: failure.Kind, File: failure.File, Detail: failure.Detail,
			Rendition: failure.Rendition, Seq: failure.Seq,
		})
	}
	if err := report.WriteJSON(*out); err != nil {
		return failPath(err)
	}
	printSummary(report, started)
	if !report.RoundTripOK {
		return verify.ExitRoundTripFailed
	}
	if len(report.Failures) > 0 || report.Gaps > 0 {
		return verify.ExitCheckFailed
	}
	return verify.ExitOK
}

func printSummary(report *verify.Report, started time.Time) {
	fmt.Fprintf(os.Stderr, "verify: renditions=%d segments=%d gaps=%d drift_max_ms=%d failures=%d in %s\n",
		report.Renditions, report.Segments, report.Gaps, report.DriftMaxMS,
		len(report.Failures), time.Since(started).Round(time.Millisecond))
	for _, stats := range report.RenditionStats {
		fmt.Fprintf(os.Stderr, "  r%d %-5s %8d bps %s segments=%d gaps=%d drift_max_ms=%d\n",
			stats.Rendition, stats.Name, stats.BitrateBps, stats.Resolution,
			stats.Segments, stats.Gaps, stats.MaxDriftMS)
	}
}

func failUsage(message string) int {
	fmt.Fprintf(os.Stderr, "usage error: %s\n", message)
	return verify.ExitUsage
}

func failPath(err error) int {
	fmt.Fprintf(os.Stderr, "path or i/o error: %v\n", err)
	return verify.ExitUsage
}

func failRoundTrip(err error) int {
	fmt.Fprintf(os.Stderr, "round-trip error: %v\n", err)
	return verify.ExitRoundTripFailed
}
