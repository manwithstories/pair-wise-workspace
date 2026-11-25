package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"

	"mediacheck/internal/ingest"
	"mediacheck/internal/media"
	"mediacheck/internal/mux"
	"mediacheck/internal/stream"
)

func routeVideoThroughQueue(ctx context.Context, opts Options, units []ingest.Unit,
	codec ingest.Codec, frameDuration int64) ([]mux.Fragment, error) {
	q := stream.NewQueue(stream.Config{MaxItems: opts.QueueItems, MaxBytes: opts.QueueBytes})
	producerErr := make(chan error, 1)
	go func() {
		defer q.Close()
		for _, u := range units {
			sample := mux.AnnexBToLengthPrefixed(u.Payload)
			item := stream.Item{
				Offset: u.Offset, Length: int(u.HeaderLength) + len(u.Payload),
				PTS: u.PTS, Payload: sample,
			}
			if err := q.Put(ctx, item); err != nil {
				producerErr <- err
				return
			}
		}
	}()

	var fragments []mux.Fragment
	var current *mux.Fragment
	for {
		item, err := q.Get(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		key := itemIsKeyframe(item.Payload, codec)
		if key {
			if current != nil && len(current.Samples) > 0 {
				fragments = append(fragments, *current)
			}
			current = &mux.Fragment{TrackID: 1, BasePTS: item.PTS}
		}
		if current == nil {
			current = &mux.Fragment{TrackID: 1, BasePTS: item.PTS}
		}
		duration := frameDuration
		current.Samples = append(current.Samples, mux.Sample{
			PTS: item.PTS, Duration: duration, Data: item.Payload, Keyframe: key,
		})
		q.Ack(item.EndOffset())
	}
	if current != nil && len(current.Samples) > 0 {
		fragments = append(fragments, *current)
	}
	select {
	case err := <-producerErr:
		if err != nil {
			return nil, err
		}
	default:
	}
	if len(fragments) == 0 {
		return nil, media.E(media.StageQueue, media.KindValidation, 0,
			fmt.Errorf("video queue produced no fragments"))
	}
	return fragments, nil
}

func itemIsKeyframe(mp4Sample []byte, codec ingest.Codec) bool {
	if len(mp4Sample) < 5 {
		return false
	}
	nal := mp4Sample[4]
	if codec == ingest.CodecH264 {
		return nal&0x1f == 5
	}
	if codec == ingest.CodecH265 {
		t := int((nal >> 1) & 0x3f)
		return t == 19 || t == 20 || t == 21
	}
	return false
}

func buildAudioFragment(pcm []byte) mux.Fragment {
	const framesPerChunk = 1024
	const channels = 2
	frameBytes := 2 * channels
	totalFrames := len(pcm) / frameBytes
	frag := mux.Fragment{TrackID: 2, BasePTS: 0}
	for startFrame := 0; startFrame < totalFrames; startFrame += framesPerChunk {
		n := framesPerChunk
		if startFrame+n > totalFrames {
			n = totalFrames - startFrame
		}
		start := startFrame * frameBytes
		end := start + n*frameBytes
		frag.Samples = append(frag.Samples, mux.Sample{
			PTS:      media.PCMTicks(int64(startFrame), 48000),
			Duration: media.PCMTicks(int64(n), 48000),
			Data:     pcm[start:end],
		})
	}
	return frag
}
