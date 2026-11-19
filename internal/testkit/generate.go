// Package testkit generates deterministic fixtures: a timeline document, one
// MP4 per track and an RTP capture with controlled loss, reordering, duplicates
// and timestamp wraps. Every fixture is reproducible from a fixed seed.
package testkit

import (
	"fmt"
	"os"
	"path/filepath"
)

// Fixture is the description of a generated archive.
type Fixture struct {
	Dir      string `json:"dir"`
	Timeline string `json:"timeline"`
	Capture  string `json:"capture"`
	// CycleTimeline is a second timeline whose audio tracks depend on each
	// other, covering the cycle-detection path without disturbing the main
	// fixture.
	CycleTimeline string   `json:"cycle_timeline"`
	Manifest      Manifest `json:"manifest"`
}

// Manifest records the expectations the generator baked in, so the CLI can
// verify them in the acceptance run.
type Manifest struct {
	Seed             int64           `json:"seed"`
	Tracks           []TrackManifest `json:"tracks"`
	CorruptByteStart int64           `json:"corrupt_byte_start"`
	CorruptByteEnd   int64           `json:"corrupt_byte_end"`
	CorruptTrack     string          `json:"corrupt_track"`
	LostSequences    []uint16        `json:"lost_sequences"`
	LateSequences    []uint16        `json:"late_sequences"`
	DuplicateCount   int             `json:"duplicate_count"`
	OutOfOrderCount  int             `json:"out_of_order_count"`
	TimestampWraps   int             `json:"timestamp_wraps"`
	VFRTrack         string          `json:"vfr_track"`
}

// TrackManifest records per-track fixture facts.
type TrackManifest struct {
	Track     string `json:"track"`
	File      string `json:"file"`
	Kind      string `json:"kind"`
	Codec     string `json:"codec"`
	Samples   int    `json:"samples"`
	Timescale uint32 `json:"timescale"`
	VFR       bool   `json:"vfr"`
}

// Config controls the generated scenario. Zero values select defaults.
type Config struct {
	Seed        int64
	OutDir      string
	VideoFrames int // default 48
	AudioChunks int // default 384 (8 s of 1024-sample frames)
	LossEvery   int // drop one packet every N; 0 uses 40
	CorruptAt   int // corrupt this video sample (0-based); default 20
}

// Generate writes the fixture and returns its description.
func Generate(cfg Config) (*Fixture, error) {
	if cfg.Seed == 0 {
		cfg.Seed = 20261120
	}
	if cfg.VideoFrames <= 0 {
		cfg.VideoFrames = 240
	}
	// The fixture needs at least two keyframe intervals and a sample after the
	// planted damage for the bracketing search to have anything to find.
	if cfg.VideoFrames < 12 {
		return nil, fmt.Errorf("video frames must be at least 12, got %d", cfg.VideoFrames)
	}
	if cfg.AudioChunks <= 0 {
		cfg.AudioChunks = 384
	}
	if cfg.LossEvery <= 0 {
		// One packet lost per 25 frames gives a 95% in-order recovery rate:
		// comfortably above the 0.90 acceptance floor while still exercising
		// real gaps, a late packet and duplicates.
		cfg.LossEvery = 25
	}
	if cfg.CorruptAt <= 0 || cfg.CorruptAt >= cfg.VideoFrames-1 {
		// Mid-GOP, never on a keyframe and never the last sample: the damage
		// must be bracketed by the previous and next decodable keyframes to
		// exercise the search.
		cfg.CorruptAt = cfg.VideoFrames/2 + 3
		if cfg.CorruptAt >= cfg.VideoFrames {
			cfg.CorruptAt = cfg.VideoFrames - 2
		}
	}
	if cfg.OutDir == "" {
		return nil, fmt.Errorf("out dir is required")
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}

	rnd := newRand(cfg.Seed)
	fx := &Fixture{
		Dir:      cfg.OutDir,
		Timeline: filepath.Join(cfg.OutDir, "timeline.json"),
		Capture:  filepath.Join(cfg.OutDir, "stream.rtp"),
	}
	fx.Manifest.Seed = cfg.Seed

	// Video: 30 fps at 90 kHz, keyframe every 10 frames.
	videoSamples := make([]SampleSpec, cfg.VideoFrames)
	for i := range videoSamples {
		videoSamples[i] = SampleSpec{Duration: 3000, Keyframe: i%10 == 0, Size: 900}
	}
	corrupt := cfg.CorruptAt
	videoSamples[corrupt].ZeroBytes = videoSamples[corrupt].Size

	videoFile := filepath.Join(cfg.OutDir, "video.mp4")
	videoOffsets, err := WriteMP4(videoFile, TrackSpec{
		TrackID: 1, Kind: "video", Codec: "avc1", Timescale: 90000,
		Width: 640, Height: 360, Seed: uint64(cfg.Seed), Samples: videoSamples,
	})
	if err != nil {
		return nil, err
	}
	fx.Manifest.CorruptByteStart = videoOffsets[corrupt]
	fx.Manifest.CorruptByteEnd = videoOffsets[corrupt] + int64(videoSamples[corrupt].Size) - 1
	fx.Manifest.CorruptTrack = "V1"
	fx.Manifest.Tracks = append(fx.Manifest.Tracks, TrackManifest{
		Track: "V1", File: "video.mp4", Kind: "video", Codec: "avc1",
		Samples: len(videoSamples), Timescale: 90000,
	})

	// Audio: 48 kHz, AAC-style 1024-sample frames, one second of bed.
	audioSamples := make([]SampleSpec, cfg.AudioChunks)
	for i := range audioSamples {
		audioSamples[i] = SampleSpec{Duration: 1024, Keyframe: true, Size: 220}
	}
	audioFile := filepath.Join(cfg.OutDir, "audio.mp4")
	if _, err := WriteMP4(audioFile, TrackSpec{
		TrackID: 2, Kind: "audio", Codec: "mp4a", Timescale: 48000,
		Channels: 2, SampleRate: 48000, Seed: uint64(cfg.Seed) + 99, Samples: audioSamples,
	}); err != nil {
		return nil, err
	}
	fx.Manifest.Tracks = append(fx.Manifest.Tracks, TrackManifest{
		Track: "A1", File: "audio.mp4", Kind: "audio", Codec: "mp4a",
		Samples: len(audioSamples), Timescale: 48000,
	})

	// Proxy: variable frame rate, deliberately uneven durations.
	proxySamples := make([]SampleSpec, 24)
	proxyDurs := []int64{3000, 4500, 1500, 6000, 3000, 7500, 2250, 3000}
	for i := range proxySamples {
		proxySamples[i] = SampleSpec{
			Duration: proxyDurs[i%len(proxyDurs)], Keyframe: i%6 == 0, Size: 320,
		}
	}
	proxyFile := filepath.Join(cfg.OutDir, "proxy.mp4")
	if _, err := WriteMP4(proxyFile, TrackSpec{
		TrackID: 3, Kind: "video", Codec: "avc1", Timescale: 90000,
		Width: 320, Height: 180, Seed: uint64(cfg.Seed) + 7, Samples: proxySamples,
	}); err != nil {
		return nil, err
	}
	fx.Manifest.Tracks = append(fx.Manifest.Tracks, TrackManifest{
		Track: "P1", File: "proxy.mp4", Kind: "proxy", Codec: "avc1",
		Samples: len(proxySamples), Timescale: 90000, VFR: true,
	})
	fx.Manifest.VFRTrack = "P1"

	// Audio master: long-form track that overshoots the video.
	masterSamples := make([]SampleSpec, cfg.AudioChunks*2)
	for i := range masterSamples {
		masterSamples[i] = SampleSpec{Duration: 1024, Keyframe: true, Size: 200}
	}
	masterFile := filepath.Join(cfg.OutDir, "master.mp4")
	if _, err := WriteMP4(masterFile, TrackSpec{
		TrackID: 4, Kind: "audio", Codec: "mp4a", Timescale: 48000,
		Channels: 2, SampleRate: 48000, Seed: uint64(cfg.Seed) + 1234, Samples: masterSamples,
	}); err != nil {
		return nil, err
	}
	fx.Manifest.Tracks = append(fx.Manifest.Tracks, TrackManifest{
		Track: "A2", File: "master.mp4", Kind: "audio", Codec: "mp4a",
		Samples: len(masterSamples), Timescale: 48000,
	})

	// A truncated second copy of the proxy exercises the truncation path
	// without touching the primary fixture.
	truncFile := filepath.Join(cfg.OutDir, "proxy-truncated.mp4")
	if err := copyFile(proxyFile, truncFile); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(truncFile); err == nil {
		if err := Truncate(truncFile, fi.Size()-int64(320*3)); err != nil {
			return nil, err
		}
	}
	fx.Manifest.Tracks = append(fx.Manifest.Tracks, TrackManifest{
		Track: "T1", File: "proxy-truncated.mp4", Kind: "proxy", Codec: "avc1",
		Samples: len(proxySamples), Timescale: 90000, VFR: true,
	})

	// RTP capture: a 90 kHz video stream that wraps the 33-bit clock once.
	capture, capManifest, err := generateCapture(cfg, rnd)
	if err != nil {
		return nil, err
	}
	if err := writeCapture(fx.Capture, capture); err != nil {
		return nil, err
	}
	fx.Manifest.LostSequences = capManifest.lost
	fx.Manifest.LateSequences = capManifest.late
	fx.Manifest.DuplicateCount = capManifest.dupes
	fx.Manifest.OutOfOrderCount = capManifest.reordered
	fx.Manifest.TimestampWraps = capManifest.wraps

	// Timeline tying the tracks and cuts together.
	tl := buildTimeline(cfg, fx.Manifest)
	if err := writeTimeline(fx.Timeline, tl); err != nil {
		return nil, err
	}
	fx.CycleTimeline = filepath.Join(cfg.OutDir, "timeline-cycle.json")
	if err := writeTimeline(fx.CycleTimeline, cycleTimeline(tl)); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(cfg.OutDir, "manifest.json"), fx.Manifest); err != nil {
		return nil, err
	}
	return fx, nil
}
