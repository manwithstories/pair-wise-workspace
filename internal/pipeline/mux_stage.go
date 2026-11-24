package pipeline

import (
	"mediarig/internal/fixture"
	"mediarig/internal/mp4"
)

// BuildMuxConfig chunks normalized PCM into 960-frame MP4 samples and maps
// restored H.264 RTP NAL units to AVCC-length-prefixed samples.
func BuildMuxConfig(pcm []byte, cap *CaptureResult, color mp4.ColorDecision) mp4.MuxConfig {
	const frameBytes = 4 // stereo s16le
	const samplesPerChunk = 960
	chunkBytes := samplesPerChunk * frameBytes
	var audioSamples [][]byte
	var audioDurations []uint32
	for len(pcm) > 0 {
		n := chunkBytes
		if n > len(pcm) {
			n = len(pcm)
		}
		chunk := append([]byte(nil), pcm[:n]...)
		audioSamples = append(audioSamples, chunk)
		audioDurations = append(audioDurations, uint32(n/frameBytes))
		pcm = pcm[n:]
	}

	var videoSamples []mp4.VideoSample
	for i, pkt := range cap.Video.Packets {
		nal := append([]byte(nil), pkt.Packet.Payload...)
		sync := i == 0 || pkt.Packet.Marker || nalType(nal) == 5
		videoSamples = append(videoSamples, mp4.VideoSample{
			NALs:  [][]byte{nal},
			Delta: 3000, // 30 fps in 90 kHz clock
			Sync:  sync,
		})
	}
	return mp4.MuxConfig{
		Audio: mp4.AudioTrack{
			SampleRate: 48000, Channels: 2, Bits: 16, Timescale: 48000,
			Samples: audioSamples, Durations: audioDurations,
		},
		Video: mp4.VideoTrack{
			Width: 1920, Height: 1080, Timescale: 90000,
			SPS: fixture.SPS(), PPS: fixture.PPS(), Samples: videoSamples,
		},
		Color: color,
	}
}

func nalType(nal []byte) byte {
	if len(nal) == 0 {
		return 0
	}
	return nal[0] & 0x1f
}
