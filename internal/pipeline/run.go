package pipeline

import (
	"fmt"
	"sort"

	"mediarun/internal/model"
	"mediarun/internal/parse"
	"mediarun/internal/testkit"
	"mediarun/internal/txn"
)

// Run performs the full demo: generate, ingest, parse, index, merge, render,
// commit, then verify from disk.
func Run(opts Options) (*Result, error) {
	if opts.Seed == 0 {
		opts.Seed = 42
	}
	if opts.Window == 0 {
		opts.Window = 64
	}
	if len(opts.Bandwidth) == 0 {
		opts.Bandwidth = []int{4}
	}
	// A demo always starts a fresh run; committed hashes from an earlier
	// seed are not comparable, and leaving them would look like a changed
	// commit. Recovery within this run still exercises the replay path.
	if err := txn.Prepare(opts.Out); err != nil {
		return nil, err
	}
	res := &Result{BandwidthSteps: append([]int(nil), opts.Bandwidth...), Scenario: string(opts.Scenario)}

	media, err := testkit.GenerateMedia(opts.Seed)
	if err != nil {
		return nil, err
	}
	res.RoundTripRate = roundTripRate(media)

	// --- corrupt-input stage: every malformed sample must be rejected ---
	corruptCases, corruptCaught, err := runCorruptStage(opts, media)
	if err != nil {
		return nil, err
	}
	res.CorruptCases = corruptCases
	res.CorruptCaught = corruptCaught
	// Every committed file must reconstruct to the same hash a direct read
	// of the bytes would produce.
	if err := checkReload(opts.Out); err != nil {
		return nil, err
	}

	// --- transport ---
	transport, err := runTransport(opts, media)
	if err != nil {
		return nil, err
	}
	res.DetectedGaps = transport.gaps

	// --- parse + index ---
	indexResult, err := runIndex(media)
	if err != nil {
		return nil, err
	}
	res.KeyframeRate = indexResult.rate

	// --- compose + commit ---
	composeResult, err := runCompose(opts, media, transport, indexResult)
	if err != nil {
		return nil, err
	}
	res.Segments = len(composeResult.segments)
	res.RollbackCount = composeResult.rollbacks
	res.Frames = composeResult.frames
	res.Events = composeResult.events

	// --- verification re-reads the files ---
	manifest, err := txn.LoadManifest(opts.Out)
	if err != nil {
		return nil, err
	}
	report, err := txn.VerifyManifest(opts.Out, manifest)
	if err != nil {
		return nil, err
	}
	for _, r := range report.Results {
		if !r.OK {
			return nil, fmt.Errorf("%w: segment %q hash mismatch", model.ErrCommit, r.Name)
		}
		res.SegmentHashes = append(res.SegmentHashes, r.Actual)
	}
	if len(res.SegmentHashes) != res.Segments || !report.AllOK() {
		return nil, fmt.Errorf("%w: %d of %d segments verified", model.ErrCommit, report.Matched(), res.Segments)
	}

	// --- recovery replay proves the invariant ---
	res.GapSummary = summarizeGaps(transport.gapList)
	rec, err := txn.Recover(opts.Out)
	if err != nil {
		return nil, err
	}
	res.Recovered = rec.Committed
	if rec.HashMatch > 0 && rec.HashMismatch == 0 {
		res.CommittedMatch = 1
	}
	// A second recovery must be identical.
	rec2, err := txn.Recover(opts.Out)
	if err != nil {
		return nil, err
	}
	if rec2.Committed != rec.Committed || rec2.HashMatch != rec.HashMatch {
		return nil, fmt.Errorf("%w: recovery is not idempotent", model.ErrCommit)
	}
	if rec.Committed == 0 {
		return nil, fmt.Errorf("%w: no committed segments after recovery", model.ErrCommit)
	}
	res.Recovered = 1
	if res.Segments < 3 {
		return nil, fmt.Errorf("%w: only %d segments, want at least 3", model.ErrInvalid, res.Segments)
	}
	return res, nil
}

// roundTripRate compares events before and after a serialize/parse cycle and
// reports the fraction that survived byte-for-byte.
func roundTripRate(media *testkit.Media) float64 {
	reparsed, err := parse.ParseMovie(media.Bytes)
	if err != nil {
		return 0
	}
	meta, err := parse.ExtractMetadata(reparsed)
	if err != nil {
		return 0
	}
	again, err := parse.EncodeMovie(reparsed)
	if err != nil {
		return 0
	}
	bytesDiffer := string(again) != string(media.Bytes)
	indexBefore, err := encodeEvents(media.ExpectedEvents)
	if err != nil {
		return 0
	}
	indexAfter, err := encodeEvents(meta.Events())
	if err != nil {
		return 0
	}
	return compareDigests(indexBefore, indexAfter, bytesDiffer)
}

// compareDigests is the pure comparison used for the round-trip rate: any
// byte-level difference zeroes the rate, otherwise the fraction of matching
// event digests is returned.
func compareDigests(before, after []string, bytesDiffer bool) float64 {
	if bytesDiffer || len(before) == 0 || len(before) != len(after) {
		return 0
	}
	matched := 0
	for i := range before {
		if before[i] == after[i] {
			matched++
		}
	}
	return float64(matched) / float64(len(before))
}

func encodeEvents(events []model.Event) ([]string, error) {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Digest())
	}
	sort.Strings(out)
	return out, nil
}
