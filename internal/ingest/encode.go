package ingest

import (
	"fmt"

	"mediacheck/internal/model"
	"mediacheck/internal/opus"
)

// encodeFixture renders the tone and the moving pattern with the real
// encoders.  Every setting is fixed, so equal seeds produce byte-identical
// assets.
func encodeFixture(cfg Config) (*mediaAssets, error) {
	audio, audioExtra, err := encodeAudioTrack(cfg)
	if err != nil {
		return nil, err
	}
	video, videoExtra, err := encodeVideoTrack(cfg)
	if err != nil {
		return nil, err
	}
	return &mediaAssets{Audio: audio, Video: video, AudioExtra: audioExtra, VideoExtra: videoExtra}, nil
}

// encodeAudioTrack encodes a deterministic stereo tone as Opus.  In-band FEC
// is enabled so the recovery stage has redundancy to work with.
func encodeAudioTrack(cfg Config) ([]encodedAudio, []byte, error) {
	if cfg.Codec == model.CodecAAC {
		return encodeAACTrack(cfg)
	}
	enc, err := opus.NewEncoder(AudioSampleRate, AudioChannels, 64000, opus.ApplicationVoIP, true, 25)
	if err != nil {
		return nil, nil, fmt.Errorf("ingest: opus encoder: %w", err)
	}
	defer enc.Close()

	span := audioSpanSamples(cfg)
	out := make([]encodedAudio, 0, span/480)
	pos := 0
	for i := 0; pos < span; i++ {
		samples := opusFrameSamples(cfg, i)
		if pos+samples > span {
			// Trim the final frame to the remaining span when a legal Opus
			// frame size fits; otherwise drop it.  Audio must never run
			// meaningfully past the video, or the sync metric reports drift
			// that is an artefact of the fixture, not the pipeline.
			samples = span - pos
			switch {
			case samples >= 960:
				samples = 960
			case samples >= 480:
				samples = 480
			case samples >= 240:
				samples = 240
			case samples >= 120:
				samples = 120
			default:
				break
			}
			if samples < 120 {
				break
			}
		}
		pcm := toneFrame(samples, pos, cfg.Seed)
		pkt, err := enc.Encode(pcm, samples)
		if err != nil {
			return nil, nil, fmt.Errorf("ingest: encoding audio frame %d: %w", i, err)
		}
		out = append(out, encodedAudio{Data: pkt, Samples: samples})
		pos += samples
		if len(out) > MaxPackets {
			return nil, nil, fmt.Errorf("ingest: audio packet cap %d: %w", MaxPackets, model.ErrLimitExceeded)
		}
	}
	return out, opus.OpusHead(AudioChannels, enc.Lookahead(), AudioSampleRate), nil
}

func encodeVideoTrack(cfg Config) ([]encodedVideo, []byte, error) {
	enc, err := newH264Encoder(VideoWidth, VideoHeight, VideoFPSNum, VideoFPSDen, cfg.KeyframeInterval)
	if err != nil {
		return nil, nil, err
	}
	defer enc.Close()
	out := make([]encodedVideo, 0, cfg.VideoFrames)
	for i := 0; i < cfg.VideoFrames; i++ {
		annexB, key, err := enc.EncodeFrame(i, cfg.Seed)
		if err != nil {
			return nil, nil, fmt.Errorf("ingest: encoding frame %d: %w", i, err)
		}
		avcc, types, err := annexBToAVCC(annexB)
		if err != nil {
			return nil, nil, fmt.Errorf("ingest: framing frame %d: %w", i, err)
		}
		out = append(out, encodedVideo{Data: avcc, Keyframe: key, NALTypes: types})
	}
	return out, enc.Extradata(), nil
}

// encodeAACTrack encodes the same tone as AAC-LC in ADTS framing.  ADTS
// packets always carry 1024 samples per frame, so the duration variation the
// Opus path exercises does not apply, but the TOC/ADTS parsing and the
// muxer's header stripping are still covered.
func encodeAACTrack(cfg Config) ([]encodedAudio, []byte, error) {
	enc, err := newAACEncoder(AudioSampleRate, AudioChannels, 128000)
	if err != nil {
		return nil, nil, err
	}
	defer enc.Close()
	span := audioSpanSamples(cfg)
	out := make([]encodedAudio, 0, span/1024)
	appendPackets := func(pkts [][]byte, samples int) error {
		for _, pkt := range pkts {
			out = append(out, encodedAudio{Data: pkt, Samples: samples})
			if len(out) > MaxPackets {
				return fmt.Errorf("ingest: audio packet cap %d: %w", MaxPackets, model.ErrLimitExceeded)
			}
		}
		return nil
	}
	pos := 0
	for pos+enc.frameSize <= span {
		samples := enc.frameSize
		pkts, err := enc.Encode(tonePlanes(samples, pos, cfg.Seed), samples)
		if err != nil {
			return nil, nil, fmt.Errorf("ingest: encoding AAC frame at %d: %w", pos, err)
		}
		if err := appendPackets(pkts, samples); err != nil {
			return nil, nil, err
		}
		pos += samples
	}
	tail, err := enc.Flush()
	if err != nil {
		return nil, nil, err
	}
	if err := appendPackets(tail, enc.frameSize); err != nil {
		return nil, nil, err
	}
	return out, enc.Extradata(), nil
}
