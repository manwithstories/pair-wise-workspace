package fixture

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// Synthesis parameters for the demo material: 10 seconds, 30 fps video with
// a keyframe every second, two bitrate rungs (~450k and ~900k) and one
// 48 kHz audio track.
const (
	videoTimescale = 90000
	audioTimescale = 48000
	videoFPS       = 30
	videoSeconds   = 10
	keyframeEvery  = videoFPS // 1 second
	videoSamples   = videoFPS * videoSeconds
	audioSamples   = 469 // 469 * 1024 / 48000 ~ 10.0s
)

// videoData builds a deterministic AVCC sample: 4-byte length + one NAL.
// IDR samples carry NAL type 5 so the keyframe scanner can recover them.
func videoData(track uint32, idx int, size uint32, idr bool) []byte {
	if size < 5 {
		size = 5
	}
	buf := make([]byte, size)
	binary.BigEndian.PutUint32(buf[:4], size-4)
	if idr {
		buf[4] = 0x65
	} else {
		buf[4] = 0x41
	}
	for i := 5; i < len(buf); i++ {
		buf[i] = byte((int(track)*31 + idx*7 + i) % 251)
	}
	return buf
}

func audioData(track uint32, idx int, size uint32) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte((int(track)*17 + idx*5 + i) % 239)
	}
	return buf
}

// videoTrack builds one video rung. avgBytes is the mean sample size that
// approximates the rung's bitrate at 30 fps.
func videoTrack(id uint32, avgBytes uint32, corrupt bool) trackSpec {
	tr := trackSpec{id: id, kind: "video", timescale: videoTimescale, width: 1280, height: 720}
	for i := 0; i < videoSamples; i++ {
		kf := i%keyframeEvery == 0
		size := avgBytes
		if kf {
			size = avgBytes * 3 / 2
		}
		s := sampleSpec{
			delta:    videoTimescale / videoFPS,
			size:     size,
			keyframe: kf,
			cto:      videoTimescale / videoFPS, // constant one-frame delay
			data:     videoData(id, i, size, kf),
		}
		if corrupt && id == 1 {
			switch i {
			case 150: // presentation timestamp rollback
				s.cto = -450000
			case 200: // damaged sample table entry
				s.size = 0
				s.data = nil
			}
		}
		tr.samples = append(tr.samples, s)
	}
	return tr
}

func audioTrack(id uint32) trackSpec {
	tr := trackSpec{id: id, kind: "audio", timescale: audioTimescale}
	for i := 0; i < audioSamples; i++ {
		tr.samples = append(tr.samples, sampleSpec{
			delta: 1024,
			size:  341,
			data:  audioData(id, i, 341),
		})
	}
	return tr
}

// Tracks returns the standard multi-bitrate track set, optionally with
// injected corruption on video track 1.
func Tracks(corrupt bool) []trackSpec {
	return []trackSpec{
		videoTrack(1, 1875, corrupt), // ~450 kbps
		videoTrack(2, 3750, corrupt), // ~900 kbps
		audioTrack(3),
	}
}

// Generate writes all sample files into dir: valid.mov, truncated.mov,
// noindex.mov and corrupt.mov.
func Generate(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	valid := buildMP4(1000, Tracks(false), false)
	files := map[string][]byte{
		"valid.mov":     valid,
		"truncated.mov": valid[:len(valid)*7/10],
		"noindex.mov":   buildMP4(1000, Tracks(false), true),
		"corrupt.mov":   buildMP4(1000, Tracks(true), false),
	}
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}
