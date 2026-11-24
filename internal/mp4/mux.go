package mp4

import (
	"bytes"
	"encoding/binary"
	"io"

	gomp4 "github.com/abema/go-mp4"

	"mediarig/internal/media"
)

// Mux writes a sequential fast-start-independent MP4: ftyp, mdat, moov.
// Sample offsets are known while mdat is written, so stco contains exact
// absolute offsets and no post-hoc chunk patching is required.
func Mux(w io.WriteSeeker, cfg MuxConfig) (*Validation, error) {
	if len(cfg.Audio.Samples) == 0 {
		return nil, media.Internal(media.StageMP4, "no audio samples to mux")
	}
	if len(cfg.Video.Samples) == 0 {
		return nil, media.Internal(media.StageMP4, "no video samples to mux")
	}
	mw := gomp4.NewWriter(w)

	if err := writeFtyp(mw); err != nil {
		return nil, err
	}

	mdatInfo, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeMdat()})
	if err != nil {
		return nil, media.InternalWrap(media.StageMP4, err, "start mdat")
	}
	audioOffset := uint32(mdatInfo.Offset + mdatInfo.HeaderSize)
	var audioBytes uint64
	audioSizes := make([]uint32, len(cfg.Audio.Samples))
	for i, s := range cfg.Audio.Samples {
		n, err := mw.Write(s)
		if err != nil {
			return nil, media.InternalWrap(media.StageMP4, err, "write audio sample %d", i)
		}
		audioSizes[i] = uint32(n)
		audioBytes += uint64(n)
	}
	videoOffset := audioOffset + uint32(audioBytes)
	videoPayloads := make([][]byte, len(cfg.Video.Samples))
	videoSizes := make([]uint32, len(cfg.Video.Samples))
	var syncSamples []uint32
	var videoBytes uint64
	for i, vs := range cfg.Video.Samples {
		var buf bytes.Buffer
		for _, nalu := range vs.NALs {
			if len(nalu) == 0 {
				return nil, media.Corrupt(media.StageMP4, "empty H.264 NAL in sample %d", i+1)
			}
			var l [4]byte
			binary.BigEndian.PutUint32(l[:], uint32(len(nalu)))
			buf.Write(l[:])
			buf.Write(nalu)
		}
		videoPayloads[i] = buf.Bytes()
		n, err := mw.Write(videoPayloads[i])
		if err != nil {
			return nil, media.InternalWrap(media.StageMP4, err, "write video sample %d", i)
		}
		videoSizes[i] = uint32(n)
		videoBytes += uint64(n)
		if vs.Sync {
			syncSamples = append(syncSamples, uint32(i+1))
		}
	}
	mdatEnd, err := mw.EndBox()
	if err != nil {
		return nil, media.InternalWrap(media.StageMP4, err, "end mdat")
	}

	audioDurations := cfg.Audio.Durations
	if len(audioDurations) != len(cfg.Audio.Samples) {
		audioDurations = make([]uint32, len(cfg.Audio.Samples))
		for i := range audioDurations {
			audioDurations[i] = 960
		}
	}
	var audioDuration uint64
	for _, d := range audioDurations {
		audioDuration += uint64(d)
	}
	videoDeltas := make([]uint32, len(cfg.Video.Samples))
	for i, s := range cfg.Video.Samples {
		videoDeltas[i] = s.Delta
		if videoDeltas[i] == 0 {
			videoDeltas[i] = 3000
		}
	}
	var videoDuration uint64
	for _, d := range videoDeltas {
		videoDuration += uint64(d)
	}

	if _, err := mw.StartBox(&gomp4.BoxInfo{Type: gomp4.BoxTypeMoov()}); err != nil {
		return nil, media.InternalWrap(media.StageMP4, err, "start moov")
	}
	timescale := uint32(1000)
	movieDuration := uint64(media.SecondsToSamples(
		media.SamplesToSeconds(int64(audioDuration), int(cfg.Audio.Timescale)), int(timescale)))
	if err := writeMvhd(mw, timescale, uint32(movieDuration), uint32(2)); err != nil {
		return nil, err
	}
	if err := writeAudioTrak(mw, cfg, audioDurations, audioSizes, audioDuration, audioOffset); err != nil {
		return nil, err
	}
	if err := writeVideoTrak(mw, cfg, videoDeltas, videoSizes, syncSamples, videoDuration, videoOffset); err != nil {
		return nil, err
	}
	if _, err := mw.EndBox(); err != nil {
		return nil, media.InternalWrap(media.StageMP4, err, "end moov")
	}

	if _, err := w.Seek(0, io.SeekEnd); err != nil {
		return nil, media.InternalWrap(media.StageMP4, err, "seek end after mux")
	}
	return &Validation{
		AudioSamples:  len(cfg.Audio.Samples),
		VideoSamples:  len(cfg.Video.Samples),
		AudioDuration: audioDuration,
		VideoDuration: videoDuration,
		MDATSize:      mdatEnd.Size,
	}, nil
}
