// Package fixture generates the self-contained sample data used by the
// demo and the end-to-end tests: normal segments, a truncated segment,
// duplicated cross-track content, a manifest gap and an RTP timestamp
// overflow sequence. Everything is deterministic for a given seed.
package fixture

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"mediacore/internal/manifest"
	"mediacore/internal/media"
)

const (
	VideoSegs       = 8
	AudioSegs       = 8
	ReplaySegs      = 4
	SegSeconds      = 2
	VideoTimescale  = 90000
	AudioTimescale  = 48000
	AudioSampleRate = 24000 // int16 samples per second inside the payload
	VideoSegBytes   = 204800
	AudioSegBytes   = 96000
	VideoSamplesPer = 50

	// GapAudioSegment is declared as EXT-X-GAP in HLS and as a
	// SegmentTimeline hole in DASH; no media file exists for it.
	GapAudioSegment = 4
	// TruncatedVideoSegment is cut short on disk.
	TruncatedVideoSegment = 5
	// CopiedSegment0/1: replay segments 0 and 1 copy these v1 segments.
	CopiedSegment0 = 2
	CopiedSegment1 = 3
)

// VideoTargetKbps and AudioTargetKbps define the bitrate target.
const (
	VideoTargetKbps = float64(VideoSegBytes) * 8 / SegSeconds / 1000 // 819.2
	AudioTargetKbps = float64(AudioSegBytes) * 8 / SegSeconds / 1000 // 384
)

// Result describes the generated data set.
type Result struct {
	Dir           string
	Manifests     []string // base names of playlist files
	SegmentFiles  []string // media files, relative to Dir
	VideoMeta     manifest.TrackMeta
	AudioMeta     manifest.TrackMeta
	ReplayMeta    manifest.TrackMeta
}

// Generate writes the full data set under dir.
func Generate(dir string, seed int64) (*Result, error) {
	rng := rand.New(rand.NewSource(seed))
	res := &Result{
		Dir:        dir,
		VideoMeta:  manifest.TrackMeta{ID: "v1", Kind: "video", Codec: "avc1", Timescale: VideoTimescale},
		AudioMeta:  manifest.TrackMeta{ID: "a1", Kind: "audio", Codec: "mp4a", Timescale: AudioTimescale},
		ReplayMeta: manifest.TrackMeta{ID: "v2", Kind: "video", Codec: "avc1", Timescale: VideoTimescale},
	}
	mediaDir := filepath.Join(dir, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return nil, err
	}

	// Video payloads: sizes jittered by whole fingerprint windows so
	// cross-track window alignment is preserved.
	videoPayloads := make([][]byte, VideoSegs)
	videoSizes := make([]int, VideoSegs)
	for i := range videoPayloads {
		size := VideoSegBytes + (rng.Intn(3)-1)*8192
		videoSizes[i] = size
		videoPayloads[i] = randomBytes(rng, size)
	}
	replayPayloads := make([][]byte, ReplaySegs)
	replayPayloads[0] = videoPayloads[CopiedSegment0]
	replayPayloads[1] = videoPayloads[CopiedSegment1]
	for i := 2; i < ReplaySegs; i++ {
		replayPayloads[i] = randomBytes(rng, VideoSegBytes)
	}

	write := func(name string, decodeTime uint64, samples []media.Sample, payload []byte) error {
		var buf bytes.Buffer
		if err := media.WriteSegment(&buf, decodeTime, samples, payload); err != nil {
			return err
		}
		path := filepath.Join(mediaDir, name)
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			return err
		}
		res.SegmentFiles = append(res.SegmentFiles, filepath.Join("media", name))
		return nil
	}

	for i := 0; i < VideoSegs; i++ {
		err := write(fmt.Sprintf("v1-%d.m4s", i+1), uint64(i*SegSeconds*VideoTimescale),
			videoSamples(videoSizes[i]), videoPayloads[i])
		if err != nil {
			return nil, err
		}
	}
	for i := 0; i < ReplaySegs; i++ {
		err := write(fmt.Sprintf("v2-%d.m4s", i+1), uint64(i*SegSeconds*VideoTimescale),
			videoSamples(len(replayPayloads[i])), replayPayloads[i])
		if err != nil {
			return nil, err
		}
	}
	audioNum := 0
	for i := 0; i < AudioSegs; i++ {
		if i == GapAudioSegment {
			continue
		}
		audioNum++
		payload := audioPayload(i)
		err := write(fmt.Sprintf("a1-%d.m4s", audioNum), uint64(i*SegSeconds*AudioTimescale),
			[]media.Sample{{Duration: SegSeconds * AudioTimescale, Size: AudioSegBytes, Keyframe: true}},
			payload)
		if err != nil {
			return nil, err
		}
	}

	// Truncate one video segment after all files are written.
	trunc := filepath.Join(mediaDir, fmt.Sprintf("v1-%d.m4s", TruncatedVideoSegment+1))
	data, err := os.ReadFile(trunc)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(trunc, data[:len(data)*3/5], 0o644); err != nil {
		return nil, err
	}

	if err := writeManifests(dir, res); err != nil {
		return nil, err
	}
	if err := writeRTP(dir); err != nil {
		return nil, err
	}
	return res, nil
}

func videoSamples(total int) []media.Sample {
	out := make([]media.Sample, VideoSamplesPer)
	base := total / VideoSamplesPer
	for i := range out {
		out[i] = media.Sample{Duration: VideoTimescale / VideoSamplesPer, Size: uint32(base)}
	}
	out[0].Size += uint32(total - base*VideoSamplesPer)
	out[0].Keyframe = true
	out[VideoSamplesPer/2].Keyframe = true
	return out
}

// audioPayload returns 2s of deterministic 24kHz int16 audio with
// fixed silence regions (segment 2: 0.5-1.8s, segment 6: 0-1.0s) and a
// sub-window silent tail in the last segment.
func audioPayload(seg int) []byte {
	n := AudioSegBytes / 2 // int16 samples
	out := make([]byte, AudioSegBytes)
	silent := func(i int) bool {
		switch seg {
		case 2:
			return i >= 12000 && i < 43200
		case 6:
			return i < 24000
		case AudioSegs - 1:
			return i >= n-200 // shorter than the analysis window
		}
		return false
	}
	for i := 0; i < n; i++ {
		var v int16
		if !silent(i) {
			v = 8000
			if (i/60)%2 == 1 {
				v = -8000
			}
		}
		out[2*i] = byte(v)
		out[2*i+1] = byte(v >> 8)
	}
	return out
}

func randomBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}
