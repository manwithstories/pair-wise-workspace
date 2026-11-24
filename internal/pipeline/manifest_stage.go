package pipeline

import (
	"math/big"

	"mediarig/internal/audio"
	"mediarig/internal/capture"
	"mediarig/internal/media"
	"mediarig/internal/mp4"
	"mediarig/internal/report"
	"mediarig/internal/txn"
)

// CaptureArtifact is the checksummed capture-stage summary.
type CaptureArtifact struct {
	Records          int                  `json:"records"`
	Tracks           []capture.TrackStats `json:"tracks"`
	LostPackets      int                  `json:"lost_packets"`
	ReorderedPackets int                  `json:"reordered_packets"`
	DuplicatePackets int                  `json:"duplicate_packets"`
}

func writeCaptureArtifact(p *txn.Paths, capRes *CaptureResult) (txn.Artifact, error) {
	art := CaptureArtifact{Records: capRes.Records}
	for _, tr := range capRes.Assembler.Tracks {
		art.Tracks = append(art.Tracks, tr.Stats)
	}
	art.LostPackets, art.ReorderedPackets, art.DuplicatePackets = capRes.TotalCounters()
	if err := txn.WriteJSONAtomic(p.CaptureJSON, art); err != nil {
		return txn.Artifact{}, err
	}
	d, n, err := txn.SHA256File(p.CaptureJSON)
	if err != nil {
		return txn.Artifact{}, err
	}
	return txn.Artifact{Name: "capture.json", Path: p.CaptureJSON, SHA256: d, Size: n}, nil
}

func buildManifestAndStats(
	p *txn.Paths,
	params txn.Params,
	inputDigest, paramsDigest string,
	capRes *CaptureResult,
	loud audio.LoudnessReport,
	color mp4.ColorDecision,
	validation *mp4.Validation,
	pcm []byte,
) (report.Manifest, report.Stats, error) {
	frames := int64(len(pcm) / 4)
	duration := media.SamplesToSeconds(frames, 48000)
	lost, reordered, duplicates := capRes.TotalCounters()

	var tracks []report.TrackTimeline
	maxSkew := media.MustRat(0, 1)
	for ssrc, tr := range capRes.Assembler.Tracks {
		tick := uint64(960)
		if tr.Spec.Format == capture.PayloadH264 {
			tick = 3000
		} else if tr.Spec.ClockRate > 0 {
			tick = uint64(tr.Spec.ClockRate / 50)
		}
		start := media.MustRat(0, 1)
		if len(tr.Packets) > 0 {
			var err error
			start, err = capRes.Clock.Relative(ssrc, tr.Packets[0].ExtTimestamp)
			if err != nil {
				return report.Manifest{}, report.Stats{}, err
			}
		}
		dur := capture.TrackEnd(tr, tick)
		d48 := media.SecondsToSamples(dur, 48000)
		startSamples := media.SecondsToSamples(start, 48000)
		skew := absRat(media.MustRat(startSamples, 1))
		if d := absRat(media.MustRat(d48-frames, 1)); d.Cmp(skew) > 0 {
			skew = d
		}
		if skew.Cmp(maxSkew) > 0 {
			maxSkew = skew
		}
		tracks = append(tracks, report.TrackTimeline{
			SSRC:           ssrc,
			Kind:           kindForFormat(tr.Spec.Format),
			Payload:        string(tr.Spec.Format),
			ClockRate:      tr.Spec.ClockRate,
			StartMediaTime: start.String(),
			Duration:       dur.String(),
			Duration48k:    d48,
			Packets:        tr.Stats.UniquePackets,
			MissingPackets: tr.Stats.MissingPackets,
		})
	}
	report.SortTracks(tracks)
	skewUS := report.SkewUS(maxSkew)
	actualKbps := ActualPCMBitrateKbps(pcm)
	targetKbps := params.TargetKbps
	if targetKbps == 0 {
		targetKbps = 1536
	}
	stats := report.Stats{
		TargetKbps:       targetKbps,
		ActualKbps:       actualKbps,
		DeviationPct:     deviationPct(actualKbps, targetKbps),
		LostPackets:      lost,
		ReorderedPackets: reordered,
		DuplicatePackets: duplicates,
		LUFSBefore:       loud.LUFSBefore,
		LUFSAfter:        loud.LUFSAfter,
		TruePeakDBTP:     loud.TruePeakDBTPAfter,
		ManifestSkewUS:   skewUS,
		Commit:           "true",
		Committed:        true,
		ClippedSamples:   loud.ClippedSamples,
		SilenceRegions:   len(loud.SilenceRegions),
	}
	manifest := report.Manifest{
		SchemaVersion:   1,
		Commit:          "true",
		InputSHA256:     inputDigest,
		ParamsSHA256:    paramsDigest,
		DurationSamples: frames,
		DurationSeconds: duration.String(),
		MaxSkewSamples:  maxSkew.String(),
		MaxSkewUS:       skewUS,
		Tracks:          tracks,
		Color:           color,
		Loudness:        loud,
		MP4Validation:   validation,
		TargetKbps:      targetKbps,
		ActualKbps:      actualKbps,
	}
	return manifest, stats, nil
}

func kindForFormat(f capture.PayloadFormat) string {
	if f == capture.PayloadH264 {
		return "video"
	}
	return "audio"
}

func absRat(r media.Rational) media.Rational {
	if r.N < 0 {
		r.N = -r.N
	}
	// Guard against MinInt64 in bounded media values.
	if r.N < 0 {
		bi := big.NewInt(r.N)
		bi.Neg(bi)
		r.N = bi.Int64()
	}
	return r
}
