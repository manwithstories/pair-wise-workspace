package main

import (
	"context"
	"fmt"
	"math"

	"fieldqc/internal/audio"
	"fieldqc/internal/clock"
	"fieldqc/internal/color"
	"fieldqc/internal/diagnose"
	"fieldqc/internal/fault"
	"fieldqc/internal/fixture"
	"fieldqc/internal/isobmff"
	"fieldqc/internal/manifest"
	"fieldqc/internal/report"
	fqrtp "fieldqc/internal/rtp"
)

// runCase executes one demo scenario and returns its report line.
func runCase(ctx context.Context, cfg config, name string) (report.Line, error) {
	model := fault.NewModel()

	// Manifest: exact rational times, edit list, non-zero starts.
	mBytes := fixture.ManifestJSON(cfg.seed, cfg.tracks, cfg.hours, name == "edit-range")
	mf, err := manifest.Parse(mBytes)
	if err != nil {
		errs, ok := err.(manifest.Errors)
		if !ok {
			return report.Line{}, fmt.Errorf("parse manifest: %w", err)
		}
		for _, e := range errs { // reject and locate, then continue on a clean copy
			model.Add(fault.Span{Track: e.Asset + "/" + e.Field, Code: e.Code, Detail: e.Msg})
		}
		if mf, err = manifest.Parse(fixture.ManifestJSON(cfg.seed, cfg.tracks, cfg.hours, false)); err != nil {
			return report.Line{}, fmt.Errorf("parse fallback manifest: %w", err)
		}
	}
	assetDur, err := mf.Assets[0].Duration()
	if err != nil {
		return report.Line{}, err
	}

	// MP4 structure/keyframe/timestamp analysis; truncation stays local.
	mp := fixture.BuildMP4(mp4Tracks(cfg))
	data := mp.Data
	if name == "truncated-box" {
		data = data[:mp.DataStart[len(mp.DataStart)-1]+4096] // cut inside the last track
	}
	res, err := isobmff.Analyze("card0.mp4", data)
	if err != nil {
		return report.Line{}, err
	}
	for _, s := range res.Faults.Spans() {
		model.Add(s)
	}

	// RTP reorder/loss recovery and A/V sync through the bounded pipeline.
	stats, syncs, winP95, err := runWindows(ctx, cfg, name, model)
	if err != nil {
		return report.Line{}, err
	}

	// Audio: 5.1/44.1 kHz normalized to stereo/48 kHz, then cut.
	pcm := fixture.PCM(cfg.seed, 44100, audio.Layout51, 2*44100)
	norm, err := audio.Normalize(pcm, 48000, audio.LayoutStereo)
	if err != nil {
		return report.Line{}, err
	}
	cut := norm.Frames() / 2 / 1024 * 1024 // on a block boundary
	if name == "audio-edge" {
		cut += 13 // land inside a 1024-frame block
	}
	_, _, edge := audio.Cut(norm, cut, 1024)
	if sp, ok := audio.EdgeSpan("card0/cama-a", 48000, edge); ok {
		model.Add(sp)
	}

	// Color: conflicts keep per-track sources, never silently override.
	metas := fixture.ColorSet(trackNames(cfg.tracks), name == "color-conflict")
	_, conflicts := color.Reconcile(metas)
	for _, s := range color.Spans(conflicts, clock.Zero(), assetDur) {
		model.Add(s)
	}

	merged := diagnose.Merge(model.Spans())
	digest := report.DigestOf(struct {
		Spans []fault.Span `json:"spans"`
		Stats fqrtp.Stats  `json:"rtp_stats"`
	}{merged, stats})
	return report.Line{
		Case:            name,
		SyncP95Ms:       round3(report.P95(syncs)),
		LossRecoveryPct: round3(stats.RecoveryPct()),
		P95WindowMs:     round3(winP95),
		DamagedSpans:    len(merged),
		Digest:          digest,
	}, nil
}

// mp4Tracks builds the synthetic MP4 layout matching the manifest:
// half video (sparse keyframes, non-zero start), half audio.
func mp4Tracks(cfg config) []fixture.MP4Track {
	nV := cfg.tracks / 2
	vSamples := uint32(cfg.hours * 3600 * 25)
	aSamples := uint32(cfg.hours * 3600 * 48000 / 1024)
	var keys []uint32
	for k := uint32(1); k <= vSamples; k += 50 { // one keyframe every 2 s
		keys = append(keys, k)
	}
	var out []fixture.MP4Track
	for i := 0; i < nV; i++ {
		out = append(out, fixture.MP4Track{
			ID: uint32(2*i + 1), Kind: "video", Timescale: 90000,
			Deltas: []uint32{3600}, Counts: []uint32{vSamples},
			Keyframes: keys, SampleSize: 40, ChunkSamples: 250, MediaTime: 3600,
		})
	}
	for i := 0; i < cfg.tracks-nV; i++ {
		out = append(out, fixture.MP4Track{
			ID: uint32(2 * (nV + i)), Kind: "audio", Timescale: 48000,
			Deltas: []uint32{1024}, Counts: []uint32{aSamples},
			SampleSize: 8, ChunkSamples: 469, MediaTime: -1,
		})
	}
	return out
}

func trackNames(tracks int) []string {
	out := make([]string, 0, tracks)
	for i := 0; i < tracks; i++ {
		out = append(out, fmt.Sprintf("card0/track%d", i+1))
	}
	return out
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
