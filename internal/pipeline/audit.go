package pipeline

import (
	"os"
	"strings"

	"mediarig/internal/media"
	"mediarig/internal/mp4"
	"mediarig/internal/report"
	"mediarig/internal/txn"
)

// Audit verifies commit state, manifest digest, skew and MP4 boxes, then
// returns the exact archived stats line for repeatable output.
func Audit(root string) (string, error) {
	p, err := txn.Layout(root)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p.Committed); err != nil {
		if os.IsNotExist(err) {
			return "", media.Policy(media.StageCommit, "batch is not committed: %s", root)
		}
		return "", media.InternalWrap(media.StageCommit, err, "stat commit marker")
	}
	lineBytes, err := os.ReadFile(p.Stats)
	if err != nil {
		return "", media.InternalWrap(media.StageReport, err, "read archived stats")
	}
	line := strings.TrimSpace(string(lineBytes))

	manifestBytes, err := os.ReadFile(p.Manifest)
	if err != nil {
		return "", media.InternalWrap(media.StageReport, err, "read manifest")
	}
	var manifest report.Manifest
	if err := txn.ReadJSON(p.Manifest, &manifest); err != nil {
		return "", err
	}
	hash := txn.DigestBytes(manifestBytes)
	if !strings.Contains(line, "manifest_sha256="+hash) {
		return "", media.Corrupt(media.StageReport, "manifest summary does not match archived stats")
	}
	if manifest.MaxSkewUS > report.MaxAllowedSkewUS+0.001 {
		return "", media.Policy(media.StageReport, "manifest skew %.3f us exceeds %.3f us",
			manifest.MaxSkewUS, report.MaxAllowedSkewUS)
	}
	deviation := 0.0
	if manifest.TargetKbps != 0 {
		d := manifest.ActualKbps - manifest.TargetKbps
		if d < 0 {
			d = -d
		}
		deviation = d / manifest.TargetKbps * 100
	}
	if deviation > 0.1 {
		return "", media.Policy(media.StageReport, "bitrate deviation %.4f%% exceeds 0.1%%", deviation)
	}
	want := auditMuxConfig(manifest)
	if _, err := mp4.ValidateFile(p.MP4, want); err != nil {
		return "", err
	}
	return line, nil
}

func auditMuxConfig(m report.Manifest) mp4.MuxConfig {
	audioSamples := 1
	videoSamples := 1
	if m.MP4Validation != nil {
		audioSamples = m.MP4Validation.AudioSamples
		videoSamples = m.MP4Validation.VideoSamples
	}
	audioDurations := make([]uint32, audioSamples)
	audioSampleBlobs := make([][]byte, audioSamples)
	for i := range audioDurations {
		audioDurations[i] = 960
		audioSampleBlobs[i] = []byte{0, 0, 0, 0}
	}
	videoSamplesCfg := make([]mp4.VideoSample, videoSamples)
	for i := range videoSamplesCfg {
		videoSamplesCfg[i] = mp4.VideoSample{Delta: 3000}
	}
	return mp4.MuxConfig{
		Audio: mp4.AudioTrack{
			SampleRate: 48000, Channels: 2, Bits: 16, Timescale: 48000,
			Samples: audioSampleBlobs, Durations: audioDurations,
		},
		Video: mp4.VideoTrack{Timescale: 90000, Samples: videoSamplesCfg},
	}
}
