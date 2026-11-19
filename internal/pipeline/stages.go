package pipeline

import (
	"fmt"
	"sort"

	"mediasync/internal/audio"
	"mediasync/internal/fingerprint"
	"mediasync/internal/media"
	"mediasync/internal/mp4"
	"mediasync/internal/rtprepair"
	"mediasync/internal/segment"
	"mediasync/internal/subtitle"
	"mediasync/internal/testdata"
	"mediasync/internal/timeline"
)

// Run executes the full pipeline for the demo scenario and returns a report.
// The returned error is non-nil only for invalid arguments that make the run
// impossible; all recoverable input problems are reported as issues.
func Run(opts Options) (*Report, error) {
	r := &Report{Version: Version, Seed: opts.Seed}
	if opts.Frames == "" {
		opts.Frames = "25"
	}
	rate, err := timeline.ParseRate(opts.Frames)
	if err != nil {
		return nil, err
	}
	tbA, err := timeline.NewTimebase(rate, timeline.Clock{Domain: "cam-a/genlock", Ticks: 90000})
	if err != nil {
		return nil, err
	}
	rateB, err := timeline.ParseRate("30000/1001")
	if err != nil {
		return nil, err
	}
	tbB, err := timeline.NewTimebase(rateB, timeline.Clock{Domain: "cam-b/free-run", Ticks: 90000})
	if err != nil {
		return nil, err
	}

	camA := &camera{name: "cam-a", timebase: tbA}
	camB := &camera{name: "cam-b", timebase: tbB}

	// Stage 1: subtitles (one clean, one damaged).
	if t := r.begin("subtitle"); true {
		camA.subs = fixtureSubs(opts.Seed)
		badSRT := testdata.GenerateSRT(testdata.SubtitleBadTime, opts.Seed)
		badRes := subtitle.ParseSRT(badSRT, "cam-c.srt")
		camB.subs = fixtureSubsVTT(opts.Seed)
		r.collect(opts, camA.subs.Issues...)
		r.collectExpected(opts, badRes.Issues...) // damage injected on purpose
		r.collect(opts, camB.subs.Issues...)
		r.finish(t, len(badRes.Cues) == 3, fmt.Sprintf("cam-a=%d cues cam-c recovered=%d cues cam-b=%d cues",
			len(camA.subs.Cues), len(badRes.Cues), len(camB.subs.Cues)),
			len(camA.subs.Issues)+len(badRes.Issues)+len(camB.subs.Issues))
	}

	// Stage 2: MP4 index for both cameras (cam-b carries a damaged stss).
	if t := r.begin("mp4"); true {
		fileA := testdata.GenerateMP4(testdata.DefaultSpec(opts.Seed))
		specB := testdata.DefaultSpec(opts.Seed)
		specB.PaddingBytes = 64 // same payloads, different absolute offsets
		specB.Fault = testdata.FaultStssOutOfRange
		fileB := testdata.GenerateMP4(specB)
		camA.file, camB.file = fileA, fileB
		camA.tracks = mp4.Index(fileA, "cam-a.mp4")
		camB.tracks = mp4.Index(fileB, "cam-b.mp4")
		issues := 0
		for _, tr := range camA.tracks {
			r.collect(opts, dedupeIssues(tr.Issues)...)
			issues += len(tr.Issues)
		}
		for _, tr := range camB.tracks {
			// cam-b ships a deliberately damaged stss.
			r.collectExpected(opts, dedupeIssues(tr.Issues)...)
			issues += len(tr.Issues)
		}
		va, vb := camA.video(), camB.video()
		ok := va != nil && vb != nil && !va.Fatal() && !vb.Fatal()
		r.finish(t, ok, fmt.Sprintf("cam-a samples=%d keys=%d cam-b samples=%d keys=%d rebuilt=%v",
			len(va.Samples), len(va.Keyframes), len(vb.Samples), len(vb.Keyframes), vb.RebuiltKeys), issues)
	}

	// Stage 3: RTP repair with injected loss, duplicates and reordering.
	var gaps []segment.Gap
	if t := r.begin("rtp"); true {
		// A long enough capture that the fluctuating bandwidth profile cycles
		// through every declared rate while the reorder window stays bounded.
		plan := testdata.RTPPlan{Packets: 1200, PayloadSize: 1200, SSRC: 0x0BADCAFE, StartSeq: 65400,
			DuplicateEvery: 17, ReorderEvery: 11, BurstLoss: opts.PacketLoss, BurstCount: 3}
		fx := testdata.GenerateRTP(plan)
		emitted, repairer := rtprepair.Simulate(fx.Packets, rtprepair.Options{Window: opts.Window}, "cam-a.rtp")
		stats := repairer.Stats()
		r.ReceivedPackets = stats.Received
		r.EmittedPackets = stats.Emitted
		r.DuplicatePackets = stats.DuplicatePackets
		r.ReorderedPackets = stats.ReorderedPackets
		r.BurstLostPackets = stats.BurstLostPackets
		r.LostPackets = stats.LostPackets
		r.RoundtripConsistency = stats.Consistency()
		r.collect(opts, repairer.Issues()...)
		gaps = gapsFromPackets(emitted)
		ok := stats.Emitted > 0 && stats.LostPackets == fx.BurstLost && stats.DuplicatePackets >= fx.Duplicates
		shape := rtprepair.Shape(fx.Packets, rtprepair.DefaultBandwidthProfile(opts.BandwidthKbps))
		r.BandwidthKbps = opts.BandwidthKbps
		r.BandwidthWindows = len(shape.Samples)
		r.ShapedDrops = shape.DroppedCount
		r.LinkMinKbps, r.LinkMaxKbps = shape.MinKbps, shape.MaxKbps
		r.finish(t, ok, fmt.Sprintf("emitted=%d lost=%d dup=%d reordered=%d link=%d..%dkbps windows=%d",
			stats.Emitted, stats.LostPackets, stats.DuplicatePackets, stats.ReorderedPackets,
			shape.MinKbps, shape.MaxKbps, len(shape.Samples)), 0)
	}

	// Stage 4: silence detection on generated PCM.
	audioMatches := 0
	if t := r.begin("audio"); true {
		pcmPlan := testdata.DefaultPCMPlan()
		pcmPlan.Seed = opts.Seed
		samples := testdata.GeneratePCM(pcmPlan)
		camA.pcm, camA.pcmFmt = samples, audio.Format{SampleRate: pcmPlan.SampleRate, Channels: pcmPlan.Channels}
		camA.silence = audio.Detect(camA.pcm, camA.pcmFmt, audio.DefaultOptions())
		r.collect(opts, camA.silence.Issues...)
		r.SilenceRuns = len(camA.silence.Runs)
		expectedRuns := 2
		audioMatches = 0
		if len(camA.silence.Runs) == expectedRuns {
			audioMatches = 1
		}
		r.finish(t, audioMatches == 1,
			fmt.Sprintf("frames=%d silent=%.1f%% runs=%d merged_chunks=%d split_gaps=%d",
				camA.silence.Frames, camA.silence.SilentRatio()*100, len(camA.silence.Runs),
				camA.silence.MergedChunks, camA.silence.SplitGaps), len(camA.silence.Issues))
	}

	// Stage 5: segmentation for every camera.
	if t := r.begin("segment"); true {
		segIssues := 0
		for _, cam := range []*camera{camA, camB} {
			video := cam.video()
			if video == nil {
				r.collect(opts, media.New(media.StageSegment, media.InputMP4, media.CodeMissingEntry,
					"camera %s has no video track", cam.name))
				continue
			}
			req := segment.Request{Name: cam.name, TargetMS: opts.TargetMS, OverlapMS: opts.OverlapMS,
				MinMS: opts.TargetMS / 3, Gaps: gaps}
			res := segment.Cut(video, req)
			cam.segments = res.Segments
			// Packet loss and stss damage are injected by the fixture
			// generator, so their consequences are reported but expected.
			segIssues += len(res.Issues)
			for _, is := range res.Issues {
				switch is.Code {
				case media.CodeOffsetInsideGap, media.CodeNoKeyframe:
					r.collectExpected(opts, is)
				default:
					r.collect(opts, is)
				}
			}
		}
		r.finish(t, len(camA.segments) > 0 && len(camB.segments) > 0,
			fmt.Sprintf("cam-a=%d cam-b=%d segments", len(camA.segments), len(camB.segments)), segIssues)
	}

	// Stage 6: fingerprinting and cross-camera duplicate detection.
	if t := r.begin("fingerprint"); true {
		for _, cam := range []*camera{camA, camB} {
			video := cam.video()
			for _, seg := range cam.segments {
				clip := fingerprint.Build(cam.name, seg, video, cam.file, cam.timebase,
					cam.transcriptAt(seg.StartNS, seg.EndNS), cam.silenceBetween(seg.StartNS, seg.EndNS))
				clip.Segment = seg
				cam.clips = append(cam.clips, clip)
			}
		}
		var all []fingerprint.Clip
		for _, cam := range []*camera{camA, camB} {
			all = append(all, cam.clips...)
		}
		matches, issues := fingerprint.Compare(all)
		r.collect(opts, issues...)
		r.DuplicateMatches = len(matches)
		for _, m := range matches {
			r.Matches = append(r.Matches, MatchReport{Hash: m.Hash, Hash64: fingerprint.Short(m.Hash),
				Cameras: m.Cameras, Count: len(m.Segments)})
		}
		for _, clip := range all {
			r.Segments = append(r.Segments, SegmentReport{
				Camera: clip.Camera, Index: clip.Index,
				StartMS: clip.StartNS / 1_000_000, EndMS: clip.EndNS / 1_000_000,
				Hash: clip.Hash, Hash64: fingerprint.Short(clip.Hash),
				Degrade: strList(clip.Segment.Degrade),
			})
		}
		sort.SliceStable(r.Segments, func(i, j int) bool {
			if r.Segments[i].Camera != r.Segments[j].Camera {
				return r.Segments[i].Camera < r.Segments[j].Camera
			}
			return r.Segments[i].Index < r.Segments[j].Index
		})
		r.finish(t, true, fmt.Sprintf("hashed=%d duplicates=%d", len(all), len(matches)), len(issues))
	}

	// Stage 7: validation of the acceptance metrics.
	if t := r.begin("validate"); true {
		hit, total := keyframeHits(camA, camB)
		if total > 0 {
			r.KeyframeHitRate = float64(hit) / float64(total)
		}
		r.SegmentCount = len(r.Segments)
		for _, s := range r.Segments {
			r.SegmentSHA256 = append(r.SegmentSHA256, s.Hash)
		}
		ok := r.GatesPass() && audioMatches == 1
		r.finish(t, ok, fmt.Sprintf("roundtrip=%.4f keyframe_hit=%.4f segments=%d",
			r.RoundtripConsistency, r.KeyframeHitRate, r.SegmentCount), 0)
		if !ok && r.RoundtripConsistency == 0 && r.KeyframeHitRate == 0 {
			r.fatal = true
		}
	}

	// Silence annotations are attached after segmentation so clip windows are
	// known; the JSON keeps them for later inspection.
	for i := range r.Segments {
		cam := camA
		if r.Segments[i].Camera == "cam-b" {
			cam = camB
		}
		startNS := r.Segments[i].StartMS * 1_000_000
		endNS := r.Segments[i].EndMS * 1_000_000
		r.Segments[i].Silence = silenceLabels(cam.silenceRunsBetween(startNS, endNS))
	}
	return r, nil
}

func keyframeHits(cams ...*camera) (hit, total int) {
	for _, cam := range cams {
		video := cam.video()
		if video == nil {
			continue
		}
		for _, seg := range cam.segments {
			total++
			idx := video.KeyframeAt(video.Samples[seg.StartSample].DTSNS)
			if idx >= 0 {
				hit++
			}
		}
	}
	return hit, total
}

func join(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
