package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"mediabridge/internal/fixture"
	rtpdump "mediabridge/internal/fixture/rtpdump"
)

// DemoOptions controls the self-contained demo corpus.
type DemoOptions struct {
	Seed    int64
	Loss    float64
	Reorder float64
	RootDir string // where fixture media and manifest are written
}

// DemoReport describes the generated corpus.
type DemoReport struct {
	ManifestPath string
	Assets       int
	VideoPackets int
	AudioPackets int
	AuxPackets   int
	RTPDelivered int
	RTPLost      int
}

// GenerateDemo writes a manifest and the media it references into root. The
// corpus contains:
//   - a clean multi-track asset with sparse keyframes and an RTP aux track,
//   - an asset with a non-zero edit start,
//   - a corrupted asset (bad chunk offset) that must be rejected with a
//     location but must not stop the others.
//
// Everything is derived from opts.Seed, so repeated runs are byte-identical.
func GenerateDemo(opts DemoOptions) (*DemoReport, error) {
	if opts.Seed == 0 {
		opts.Seed = 41
	}
	if opts.RootDir == "" {
		opts.RootDir = "."
	}
	mediaDir := filepath.Join(opts.RootDir, "media")
	auxDir := filepath.Join(opts.RootDir, "aux")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(auxDir, 0o755); err != nil {
		return nil, err
	}
	rep := &DemoReport{}

	// Asset 1: clean.
	cleanOpts := fixture.DefaultMP4Options(opts.Seed)
	clean, err := fixture.GenerateMP4(mediaDir, "archive_pt1.mp4", cleanOpts)
	if err != nil {
		return nil, err
	}
	rep.VideoPackets += clean.Video.Samples
	rep.AudioPackets += clean.Audio.Samples
	rep.AuxPackets += clean.Aux.Samples

	// Asset 1 aux: RTP timecode track with loss/reorder.
	auxRep, err := rtpdump.Generate(auxDir, "timecode.rtpd", rtpdump.Options{
		Seed: opts.Seed + 1, Count: 480, Loss: opts.Loss, Reorder: opts.Reorder,
		Duplicate: 0.01, ClockRate: 90000,
	})
	if err != nil {
		return nil, err
	}
	rep.RTPDelivered += auxRep.Delivered
	rep.RTPLost += auxRep.Dropped

	// Asset 2: non-zero edit start.
	editOpts := fixture.DefaultMP4Options(opts.Seed + 2)
	editOpts.EditMediaTime = 9000
	if _, err := fixture.GenerateMP4(mediaDir, "archive_pt2.mp4", editOpts); err != nil {
		return nil, err
	}
	rep.VideoPackets += editOpts.VideoSamples
	rep.AudioPackets += editOpts.AudioSamples
	rep.AuxPackets += editOpts.AuxSamples

	// Asset 3: corrupted (bad chunk offset) — rejected but located.
	badOpts := fixture.DefaultMP4Options(opts.Seed + 3)
	badOpts.Corrupt = fixture.CorruptBadChunkOffset
	if _, err := fixture.GenerateMP4(mediaDir, "archive_bad.mp4", badOpts); err != nil {
		return nil, err
	}

	manifestJSON := fmt.Sprintf(`{
  "version": 1,
  "root_dir": ".",
  "assets": [
    {
      "id": "PT-001",
      "primary": {"format": "mp4", "path": "media/archive_pt1.mp4"},
      "auxiliary": [
        {"format": "rtp", "path": "aux/timecode.rtpd", "sample_rate": 90000}
      ],
      "mapping": {
        "video_track_id": 1,
        "audio_track_id": 2,
        "auxiliary": [
          {"name": "timecode", "mp4_track_id": 3},
          {"name": "rtp_timecode", "source": "aux/timecode.rtpd"}
        ],
        "drop_unmapped": true
      }
    },
    {
      "id": "PT-002",
      "primary": {"format": "mp4", "path": "media/archive_pt2.mp4"},
      "mapping": {
        "video_track_id": 1,
        "audio_track_id": 2,
        "auxiliary": [
          {"name": "timecode", "mp4_track_id": 3}
        ],
        "drop_unmapped": true
      }
    },
    {
      "id": "PT-003-BAD",
      "primary": {"format": "mp4", "path": "media/archive_bad.mp4"},
      "mapping": {"drop_unmapped": true}
    }
  ]
}
`)
	manifestPath := filepath.Join(opts.RootDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0o644); err != nil {
		return nil, err
	}
	rep.ManifestPath = manifestPath
	rep.Assets = 3
	return rep, nil
}
