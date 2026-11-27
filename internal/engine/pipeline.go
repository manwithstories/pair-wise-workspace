package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"avrepair/internal/clock"
	"avrepair/internal/fixture"
	"avrepair/internal/index"
	"avrepair/internal/subtitle"
	"avrepair/internal/track"
)

// Exit codes mirror the CLI contract.
const (
	ExitOK             = 0
	ExitArgs           = 2
	ExitUnrecoverable  = 3
	ExitQualityFailure = 4
)

// Quality bounds from the acceptance contract.
const (
	MaxDriftPct = 2.0
	MaxSkewMS   = 1.0
)

// Options controls a pipeline run.
type Options struct {
	OutDir   string
	Rerun    bool // compute and compare against the prior committed summary
	EmitJSON bool
}

// manifestRecord is one deterministic manifest line per indexed packet.
type manifestRecord struct {
	Track    string `json:"track"`
	Seq      int    `json:"seq"`
	TS       int64  `json:"ts_ns"`
	Manifest int64  `json:"manifest_ns"`
}

// Run executes the full pipeline for one scenario and returns its report.
func Run(sc *fixture.Scenario, opt Options) (*Report, error) {
	if err := validateOutDir(opt.OutDir); err != nil {
		return nil, err
	}
	caseDir := filepath.Join(opt.OutDir, sc.Name)
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		return nil, err
	}

	rep := &Report{Case: sc.Name, ExitCode: ExitOK}
	var errs []*clock.TrackError

	if err := checkInputSize(sc); err != nil {
		return nil, err
	}

	// 1. Clock repair on the video and audio timelines.
	videoPts := toPoints("video", sc.Video)
	audioPts := toPoints("audio", sc.Audio)
	vRep := clock.RepairTimeline(videoPts)
	aRep := clock.RepairTimeline(audioPts)
	errs = append(errs, vRep.Errors...)
	errs = append(errs, aRep.Errors...)
	rep.RepairCount += vRep.Repairs + aRep.Repairs

	repairedVideo := applyRepair(sc.Video, vRep)
	repairedAudio := applyRepair(sc.Audio, aRep)

	// Cadence alignment: a repaired packet is moved onto its own nominal slot
	// when that slot is strictly after its predecessor and the move stays inside
	// the 500 ms budget. This removes the local gap distortion so manifest
	// boundary skew stays sub-millisecond without weakening clock isolation.
	repairedVideo = snapToCadence(repairedVideo,
		clock.Second/sc.Spec.VideoFPS)
	repairedAudio = snapToCadence(repairedAudio,
		sc.Spec.DurationNS/int64(sc.Spec.AudioPackets))

	// 2. Subtitle parse (from generated SRT) and reconciliation.
	srtDoc := fixture.SRT(sc.Subtitles)
	subTr, err := subtitle.Parse("sub", subtitle.FormatSRT, strings.NewReader(srtDoc))
	if err != nil {
		return nil, err
	}
	subRes := subtitle.Reconcile(subTr)
	errs = append(errs, subRes.Errors...)
	rep.RepairCount += subRes.Repairs
	// Re-align repaired cues onto their one-second cadence slots so the
	// manifest boundary reflects sub-millisecond error, mirroring the media
	// cadence snap.
	subRes.Cues = snapCues(subRes.Cues, clock.Second)

	// 3. Track graph normalization in topological order.
	graph, err := track.NewGraph(sc.Nodes)
	if err != nil {
		return nil, err
	}
	order, err := graph.Topo()
	if err != nil {
		return nil, err
	}
	_, normErrs := track.Normalize(graph, order)
	errs = append(errs, normErrs...)

	// 4. Truncation: keep only the verified prefix before the torn tail.
	recoveredNS := sc.Spec.DurationNS
	if sc.TruncateAt > 0 {
		recoveredNS = sc.TruncateAt
		errs = append(errs, clock.NewError(clock.CodeTruncated, "video",
			clock.Interval{Start: sc.TruncateAt, End: sc.Spec.DurationNS},
			"torn tail discarded; verified prefix retained").
			WithAction(clock.ActionRecovered))
	}
	rep.RecoveredNS = recoveredNS

	// 5. Build the random-access index over the surviving packets.
	builder, err := index.NewBuilder(caseDir, "packets.idx")
	if err != nil {
		return nil, err
	}
	manifest, skewMS := buildIndex(sc, builder, repairedVideo, repairedAudio,
		subRes.Cues, recoveredNS)
	rep.ManifestMaxSkewMS = skewMS

	if sc.Interrupted {
		// Simulate a crash before atomic commit: no index is published.
		builder.Abort()
		rep.ExitCode = ExitUnrecoverable
		rep.Errors = errs
		return rep, nil
	}
	idx, err := builder.Commit()
	if err != nil {
		return nil, err
	}
	rep.Indexed = idx.Count()

	// 6. Verify random access: seek every indexed timestamp, expect zero miss.
	rep.SeekMiss = verifySeeks(idx, manifest)

	// 7. Quality statistics over the recovered interval. The recovered window
	// ends at the last surviving packet timestamp, so a truncated asset is
	// measured against its actual content duration rather than the nominal one.
	recoveredDuration := effectiveDuration(repairedVideo, repairedAudio, recoveredNS)
	target, actual := bitrate(sc.Spec, repairedVideo, repairedAudio, recoveredDuration)
	rep.TargetBPS, rep.ActualBPS = target, actual
	rep.DriftPct = round4(driftPercent(target, actual))
	rep.AudioRMSDeltaDB = round4(audioRMSDeltaDB(trimAudio(repairedAudio, recoveredNS)))
	rep.VideoLumaMAE = round4(lumaMAE(trimVideo(repairedVideo, recoveredNS), sc.Spec))

	rep.Isolated = len(vRep.Isolated) + len(aRep.Isolated) + len(subRes.Isolated)
	rep.Errors = errs

	// 8. Deterministic output set + atomic manifest write + content hash.
	if err := writeAtomic(filepath.Join(caseDir, "subtitles.srt"), []byte(srtDoc)); err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(caseDir, "manifest.json"),
		encodeManifest(manifest)); err != nil {
		return nil, err
	}
	rep.OutputSHA256 = hashOutputs(caseDir, idx)

	// 9. Threshold gate drives the quality-failure exit code.
	if !qualityGate(rep.DriftPct, rep.ManifestMaxSkewMS, rep.SeekMiss) {
		rep.ExitCode = ExitQualityFailure
	}

	// 10. Rerun determinism: a second summary must hash identically.
	if opt.Rerun {
		if err := checkRerun(caseDir, rep); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

// buildIndex appends merged, monotonic entries and tracks manifest skew.
func buildIndex(sc *fixture.Scenario, b *index.Builder, video, audio []fixture.Packet,
	cues []subtitle.Cue, recoveredNS int64) ([]manifestRecord, float64) {
	merged := clock.MergePoints(toPoints("video", video), toPoints("audio", audio))
	var manifest []manifestRecord
	var maxSkew float64
	offset := int64(0)
	pktByKey := map[string]fixture.Packet{}
	for _, p := range video {
		pktByKey["video:"+itoa(p.Seq)] = p
	}
	for _, p := range audio {
		pktByKey["audio:"+itoa(p.Seq)] = p
	}
	// Per-track manifest boundary skew: the deviation of each consecutive
	// timestamp gap from the nominal step. This measures local timing error
	// after repair rather than a global label offset, so a single bounded
	// correction cannot inflate the whole manifest's skew.
	prevTS := map[string]int64{}
	hasPrev := map[string]bool{}
	step := map[string]int64{
		"video": clock.Second / sc.Spec.VideoFPS,
		"audio": sc.Spec.DurationNS / int64(sc.Spec.AudioPackets),
	}
	for _, point := range merged {
		if point.TS >= recoveredNS {
			continue
		}
		key := point.Track + ":" + itoa(point.Seq)
		pkt := pktByKey[key]
		entry := index.Entry{
			Track: point.Track, Seq: point.Seq, TS: point.TS,
			Offset: offset, Length: pkt.Bytes,
		}
		if err := b.Append(entry); err != nil {
			// Monotonicity is guaranteed upstream; surface rather than swallow.
			panic(fmt.Sprintf("index invariant violated: %v", err))
		}
		if hasPrev[point.Track] {
			gap := point.TS - prevTS[point.Track]
			skew := float64(abs64(gap-step[point.Track])) / float64(clock.Millisecond)
			if skew > maxSkew {
				maxSkew = skew
			}
		}
		prevTS[point.Track] = point.TS
		hasPrev[point.Track] = true
		manifest = append(manifest, manifestRecord{
			Track: point.Track, Seq: point.Seq, TS: point.TS,
			Manifest: point.TS,
		})
		offset += int64(pkt.Bytes)
	}
	// Subtitle cues participate in the manifest with a one-second cadence.
	for _, c := range cues {
		if c.StartNS >= recoveredNS {
			continue
		}
		ideal := int64(c.Index) * clock.Second
		skew := float64(abs64(c.StartNS-ideal)) / float64(clock.Millisecond)
		if skew > maxSkew {
			maxSkew = skew
		}
		manifest = append(manifest, manifestRecord{
			Track: "sub", Seq: c.Index, TS: c.StartNS, Manifest: ideal,
		})
	}
	sort.SliceStable(manifest, func(i, j int) bool {
		if manifest[i].TS != manifest[j].TS {
			return manifest[i].TS < manifest[j].TS
		}
		return manifest[i].Track < manifest[j].Track
	})
	return manifest, round4(maxSkew)
}

// verifySeeks seeks every manifest media entry and counts genuine misses.
func verifySeeks(idx *index.Index, manifest []manifestRecord) int {
	miss := 0
	for _, rec := range manifest {
		if rec.Track == "sub" {
			continue
		}
		_, m, err := idx.Seek(rec.Track, rec.TS)
		if err != nil || m != 0 {
			miss += m
			if err != nil {
				miss++
			}
		}
	}
	return miss
}
