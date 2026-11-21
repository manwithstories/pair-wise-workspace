package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"mediacheck/internal/audio"
	"mediacheck/internal/clock"
	"mediacheck/internal/fingerprint"
	"mediacheck/internal/fixture"
	"mediacheck/internal/journal"
	"mediacheck/internal/metrics"
	"mediacheck/internal/mp4"
	mrtp "mediacheck/internal/rtp"
	"mediacheck/internal/split"
	"mediacheck/internal/txn"
)

// stageNames is the fixed pipeline order; crash injection uses it.
var stageNames = []string{"rtp", "parse", "timeline", "split", "silence", "fingerprint", "commit"}

// errDeterminism marks a replayed stage whose digest differs from the
// journal — the recovery/atomicity validation failure (exit 4).
var errDeterminism = errors.New("pipeline: replayed stage digest differs from journal")

// result carries everything the report needs out of the pipeline.
type result struct {
	stats             mrtp.Stats
	timeline          *clock.Timeline
	segments          []split.Segment
	avSyncP95         float64
	silentIntervals   int
	silentIssues      int
	duplicates        int
	parseFailures     int
	committedSegments int
	commitDigest      string
	commitSkipped     bool
}

// parsedFrag couples a parsed fragment with its track name.
type parsedFrag struct {
	track string
	info  *mp4.Info
}

func digestOf(parts ...interface{}) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%v\n", p)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// journalStage appends a stage record unless an identical one already
// exists; a conflicting record is a determinism violation.
func journalStage(wal *journal.WAL, stage, input, output string) error {
	if prev, ok := wal.StageOutput(stage); ok {
		if prev != output {
			return fmt.Errorf("%w: stage %s journal=%s recomputed=%s", errDeterminism, stage, prev, output)
		}
		return nil // replay: already journaled, do not duplicate
	}
	_, err := wal.Append(journal.Record{Type: journal.TypeStage, Stage: stage, Input: input, Output: output})
	return err
}

// runStages executes the pipeline in order. If stopAfter names a
// stage, it returns crashed=true right after journaling that stage,
// simulating a crash before any further work.
func runStages(cfg config, wal *journal.WAL, tx *txn.Txn, stream *fixture.Stream, stopAfter string, rep *metrics.Report) (*result, bool, error) {
	res := &result{timeline: clock.New()}
	prev := digestOf("config", cfg.seed, cfg.lossBurst, cfg.bandwidth, cfg.fixtureCase)
	crashed := false
	finish := func(stage, output string) (bool, error) {
		if err := journalStage(wal, stage, prev, output); err != nil {
			return false, err
		}
		prev = output
		return stopAfter == stage, nil
	}

	// Stage rtp: reorder, deduplicate, bounded loss recovery.
	reorderer := mrtp.NewReorderer(mrtp.Config{
		WindowSize: 64, BaseWaitMs: 80, RefBandwidthMbps: 3,
		MinWaitMs: 20, LatencyBudgetMs: 250,
	})
	ordered, stats := reorderer.Process(stream.Arrivals)
	res.stats = stats
	out := digestOf("rtp", stats.Received, stats.Duplicates, stats.Recovered, stats.Late, stats.Lost, stats.Emitted)
	if crashed, _ = finish("rtp", out); crashed {
		return res, true, nil
	}

	// Stage parse: box structure, mfhd/tfdt/trun extraction. Corrupt
	// fragments are recorded and skipped, never silently dropped.
	var parsed []parsedFrag
	for _, pkt := range ordered {
		info, err := mp4.Parse(pkt.Payload)
		if err != nil {
			res.parseFailures++
			rep.AddFailure("parse", fmt.Sprintf("rtp-seq %d: %v", pkt.SequenceNumber, err))
			continue
		}
		track := "video"
		if info.TrackID == fixture.AudioTrackID {
			track = "audio"
		}
		parsed = append(parsed, parsedFrag{track: track, info: info})
	}
	out = digestOf("parse", len(parsed), res.parseFailures)
	if crashed, _ = finish("parse", out); crashed {
		return res, true, nil
	}

	// Stage timeline: unified base, traceable gaps and rollbacks.
	samples := map[string][]uint32{}
	for _, pf := range parsed {
		var durs []uint32
		for _, s := range pf.info.Samples {
			durs = append(durs, s.Duration)
		}
		key := fmt.Sprintf("%s/%d", pf.track, pf.info.Seq)
		samples[key] = durs
		err := res.timeline.Add(clock.Frag{
			Track: pf.track, Seq: pf.info.Seq, TFDT: pf.info.TFDT,
			TFDTVersion: pf.info.TFDTVersion, Duration: pf.info.Duration(),
			Timescale: timescaleOf(pf.track), Index: len(samples),
		})
		if err != nil {
			rep.AddFailure("timeline", fmt.Sprintf("track %s seq %d: %v", pf.track, pf.info.Seq, err))
		}
	}
	out = digestOf("timeline", res.timeline.Summary())
	if crashed, _ = finish("timeline", out); crashed {
		return res, true, nil
	}

	// Stage split: cut 1 s segments per track with deterministic
	// rounding; av_sync is the P95 of per-segment start offsets.
	var totalMs float64
	trackFrags := map[string][]clock.Frag{}
	for _, tr := range res.timeline.Tracks() {
		fr := res.timeline.Frags(tr)
		trackFrags[tr] = fr
		if len(fr) > 0 {
			last := fr[len(fr)-1]
			endMs := clock.ToMs(last.TFDT+last.Duration, last.Timescale)
			if endMs > totalMs {
				totalMs = endMs
			}
		}
	}
	perTrack := map[string][]split.Segment{}
	var syncs []float64
	for _, tr := range res.timeline.Tracks() {
		var models []split.FragModel
		for _, f := range trackFrags[tr] {
			key := fmt.Sprintf("%s/%d", tr, f.Seq)
			models = append(models, split.FragModel{StartUnits: f.TFDT, Samples: samples[key]})
		}
		cutter, err := split.NewCutter(timescaleOf(tr), models)
		if err != nil {
			return res, false, err
		}
		segs, err := split.Cut(tr, cutter, totalMs, 1000)
		if err != nil {
			return res, false, err
		}
		perTrack[tr] = segs
		res.segments = append(res.segments, segs...)
	}
	// av sync per segment index across tracks.
	if av, ok := perTrack["audio"]; ok {
		if vv, ok := perTrack["video"]; ok {
			n := len(av)
			if len(vv) < n {
				n = len(vv)
			}
			for k := 0; k < n; k++ {
				d := av[k].ActStartMs - vv[k].ActStartMs
				if d < 0 {
					d = -d
				}
				syncs = append(syncs, d)
			}
		}
	}
	res.avSyncP95 = p95(syncs)
	out = digestOf("split", len(res.segments), fmt.Sprintf("%.3f", res.avSyncP95))
	if crashed, _ = finish("split", out); crashed {
		return res, true, nil
	}

	// Stage silence: per-channel energy on audio payloads.
	silentFrag := map[string]bool{}
	for _, pf := range parsed {
		if pf.track != "audio" {
			continue
		}
		offsetMs := clock.ToMs(pf.info.TFDT, fixture.AudioTimescale)
		srep := audio.Detect("audio", pf.info.Payload, fixture.AudioChannels,
			fixture.AudioPayloadRate, -50, 10, offsetMs)
		res.silentIntervals += len(srep.Intervals)
		res.silentIssues += len(srep.Issues)
		key := fmt.Sprintf("audio/%d", pf.info.Seq)
		silentFrag[key] = srep.AllSilent()
	}
	out = digestOf("silence", res.silentIntervals, res.silentIssues)
	if crashed, _ = finish("silence", out); crashed {
		return res, true, nil
	}

	// Stage fingerprint: dedup segments; short/silent segments need
	// byte-identical payloads, not just a matching digest.
	fpSegs := buildFingerprintSegments(perTrack, trackFrags, parsed, silentFrag)
	dups := fingerprint.FindDuplicates(fpSegs, 500)
	res.duplicates = len(dups)
	out = digestOf("fingerprint", res.duplicates)
	if crashed, _ = finish("fingerprint", out); crashed {
		return res, true, nil
	}

	// Stage commit: atomic publish guarded by the WAL; a committed
	// record with the same digest makes replay a no-op.
	outs := buildOutputs(res, dups)
	want := txn.Digest(outs)
	if begin, committed, found := wal.LastTxn(); found && committed && begin.Output == want {
		res.commitSkipped = true
		res.commitDigest = want
	} else {
		d, err := tx.Commit(1, outs)
		if err != nil {
			return res, false, err
		}
		res.commitDigest = d
	}
	res.committedSegments = len(res.segments)
	out = digestOf("commit", res.commitDigest, res.committedSegments)
	if _, err := finish("commit", out); err != nil {
		return res, false, err
	}
	return res, false, nil
}

func timescaleOf(track string) uint32 {
	if track == "audio" {
		return fixture.AudioTimescale
	}
	return fixture.VideoTimescale
}

func p95(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := make([]float64, len(v))
	copy(s, v)
	sort.Float64s(s)
	return s[(len(s)*95+99)/100-1]
}
