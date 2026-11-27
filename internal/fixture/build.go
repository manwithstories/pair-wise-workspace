package fixture

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/asticode/go-astiav"

	"mediacore/internal/meta"
)

// Known fixture geometry, exported so the CLI and tests can assert on it.
const (
	VideoWidth  = 160
	VideoHeight = 90
	VideoFPS    = 10
	// AudioRate is deliberately 44.1 kHz: the pipeline must resample to 48 kHz
	// rather than pass audio through untested.
	AudioRate = 44100
)

// Build generates one fixture and returns its manifest.
//
// The file contains H.264 video with one keyframe per second, stereo 44.1 kHz
// AAC audio, mov_text subtitles cued at known offsets, chapters, and — for
// MPEG-TS — a timed-metadata data track carrying ID3 and emsg samples.
// Fault injects a deliberate defect whose records the scanner must isolate.
func Build(opt Options) (Manifest, error) {
	opt, err := opt.Validate()
	if err != nil {
		return Manifest{}, err
	}
	if opt.Format == FormatMKV {
		return Manifest{}, fmt.Errorf("fixture: matroska cannot carry the timed data track; use mp4 or mpegts")
	}
	if opt.Fault == FaultStaleIndex {
		// Stale-index is a cache defect, not a media defect: build a normal
		// file and let the CLI write the index before mutating it.
		opt.Fault = FaultNone
	}
	w, err := newWriter(opt)
	if err != nil {
		return Manifest{}, err
	}
	defer w.Close()

	seconds := float64(opt.Segments)
	if seconds < 2 {
		seconds = 2
	}
	totalFrames := int(seconds * VideoFPS)
	totalUS := int64(seconds * 1_000_000)

	if err := w.AddVideo(VideoWidth, VideoHeight, VideoFPS, "h264"); err != nil {
		return Manifest{}, err
	}
	if err := w.AddAudio(AudioRate, 2); err != nil {
		return Manifest{}, err
	}
	if opt.Format == FormatMP4 {
		if err := w.AddSubtitle(); err != nil {
			return Manifest{}, err
		}
	}

	chapters := buildChapters(totalUS)
	if opt.Format == FormatMP4 {
		for _, ch := range chapters {
			if err := w.AddChapter(ch); err != nil {
				return Manifest{}, err
			}
		}
	}
	hasData := opt.Format == FormatTS
	var data *streamWriter
	if hasData {
		data, err = w.AddDataTrack()
		if err != nil {
			return Manifest{}, err
		}
	}
	if err := w.Start(); err != nil {
		return Manifest{}, err
	}

	// Audio: 1024-sample AAC frames covering the whole duration.
	const audioFrame = 1024
	audioTotal := int(seconds * AudioRate)
	for pos := 0; pos < audioTotal; pos += audioFrame {
		n := audioFrame
		if pos+n > audioTotal {
			n = audioTotal - pos
		}
		pts := int64(pos) * 1_000_000 / AudioRate
		samples := audioSamples(AudioRate, n, pos, 440, 0.12)
		if opt.Fault == FaultTruncate && pos+n >= audioTotal {
			// Drop the final samples so the last frame's declared size
			// exceeds its payload.
			samples = samples[:len(samples)-3]
		}
		if err := w.WriteAudio(pts, samples); err != nil {
			return Manifest{}, err
		}
	}

	// Video frames, one keyframe per second (GOP == fps).
	for i := 0; i < totalFrames; i++ {
		pts := int64(i) * 1_000_000 / VideoFPS
		if err := w.WriteVideo(pts, yuv420Frame(VideoWidth, VideoHeight, i)); err != nil {
			return Manifest{}, err
		}
	}

	// Subtitle cues at 200 ms and one per second (MP4).
	if w.sub != nil {
		if err := w.WriteSubtitle(200_000, 1_600_000, "edge subtitle 0"); err != nil {
			return Manifest{}, err
		}
		for sec := 1; sec < int(seconds); sec++ {
			text := fmt.Sprintf("edge subtitle %d", sec)
			if opt.Fault == FaultTruncate && sec == 2 {
				// A declared text length longer than the payload: the scanner
				// must quarantine this cue instead of emitting garbage.
				payload := make([]byte, 2+len(text))
				binary.BigEndian.PutUint16(payload, uint16(len(text)+10))
				copy(payload[2:], text)
				if err := w.writeRawSubtitle(int64(sec)*1_000_000, 900_000, payload); err != nil {
					return Manifest{}, err
				}
				continue
			}
			if err := w.WriteSubtitle(int64(sec)*1_000_000, 900_000, text); err != nil {
				return Manifest{}, err
			}
		}
	}

	// Timed metadata on the MPEG-TS data track: one ID3 tag and one emsg per
	// second, aligned with the audio timeline.
	if data != nil {
		for sec := 0; sec < int(seconds); sec++ {
			pts := int64(sec) * 1_000_000
			recs, err := metadataSamples(sec, opt.Fault == FaultTruncate && sec == 2, opt.MaxRecordBytes)
			if err != nil {
				return Manifest{}, err
			}
			for _, sample := range recs {
				if err := w.WriteData(data, pts, sample); err != nil {
					return Manifest{}, err
				}
			}
		}
	}

	if err := w.Close(); err != nil {
		return Manifest{}, err
	}

	// Post-pass: inject defects that need raw container bytes.
	switch opt.Fault {
	case FaultTimebase:
		if err := injectBadChapterTimebase(opt.Path()); err != nil {
			return Manifest{}, err
		}
	case FaultTruncate:
		// Only streamable containers survive a raw tail cut. MP4 keeps its
		// index in a trailing moov atom, so cutting it yields an unopenable
		// file that demonstrates nothing about record isolation. MP4 already
		// carries deliberately truncated subtitle/audio payloads above.
		if opt.Format == FormatTS {
			if err := truncateFile(opt.Path(), 0.85); err != nil {
				return Manifest{}, err
			}
		}
	}

	digest, size, err := DigestFile(opt.Path())
	if err != nil {
		return Manifest{}, err
	}
	return Manifest{
		Name: opt.Name, Path: opt.Path(), Size: size, SHA256: digest,
		Segments: opt.Segments, Seconds: seconds,
		HasVideo: true, HasAudio: true, HasSubs: w.sub != nil,
		HasChapters: opt.Format == FormatMP4, HasData: data != nil,
		Notes: string(opt.Fault),
	}, nil
}

// buildChapters divides the duration into up to four equal chapters.
func buildChapters(totalUS int64) []meta.Chapter {
	titles := []string{"Intro", "Main", "Outro", "Credits"}
	n := 4
	for n > 1 && totalUS/int64(n) < 500_000 {
		n--
	}
	length := totalUS / int64(n)
	out := make([]meta.Chapter, 0, n)
	for i := 0; i < n; i++ {
		start := int64(i) * length
		end := start + length
		if i == n-1 {
			end = totalUS
		}
		out = append(out, meta.Chapter{ID: int64(i + 1), Title: titles[i%len(titles)], StartUs: start, EndUs: end})
	}
	return out
}

// metadataSamples builds the ID3 and emsg samples for one second. A truncated
// sample declares more bytes than it carries.
func metadataSamples(sec int, corrupt bool, limit int) ([][]byte, error) {
	title := fmt.Sprintf("cue-%02d", sec)
	tag := &meta.ID3Tag{Version: 4}
	tag.Frames = append(tag.Frames, meta.ID3Frame{ID: "TIT2", Data: append([]byte{3}, []byte(title)...)})
	id3, err := tag.Encode()
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(id3) > limit {
		return nil, fmt.Errorf("fixture: id3 %d bytes exceeds limit %d", len(id3), limit)
	}
	emsg := &meta.Emsg{
		Version: 0, SchemeIDURI: "urn:mediacore:emsg", Value: fmt.Sprintf("%d", sec),
		Timescale: 1000, PTDelta: uint32(sec * 1000), EventDuration: 900, ID: uint32(sec + 1),
		Message: id3,
	}
	emsgBody, err := emsg.Encode()
	if err != nil {
		return nil, err
	}
	// Length-prefixed ID3 as MPEG-TS timed ID3 carries it.
	tsID3 := make([]byte, 4, 4+len(id3))
	binary.BigEndian.PutUint32(tsID3, uint32(len(id3)))
	tsID3 = append(tsID3, id3...)
	// Boxed emsg as a bin_data sample.
	box := make([]byte, 8, 8+len(emsgBody))
	binary.BigEndian.PutUint32(box, uint32(8+len(emsgBody)))
	copy(box[4:8], "emsg")
	box = append(box, emsgBody...)
	if corrupt {
		// Declare a longer ID3 length than the payload carries.
		binary.BigEndian.PutUint32(tsID3, uint32(len(id3)+64))
	}
	return [][]byte{tsID3, box}, nil
}

// writeRawSubtitle writes an already-framed mov_text payload.
func (w *writer) writeRawSubtitle(ptsUS, durUS int64, payload []byte) error {
	if w.sub == nil {
		return nil
	}
	p := astiav.AllocPacket()
	defer p.Free()
	if err := p.FromData(payload); err != nil {
		return err
	}
	p.SetStreamIndex(w.sub.index)
	tb := w.sub.stream.TimeBase()
	den := int64(tb.Den())
	p.SetPts(ptsUS * den / 1_000_000)
	p.SetDts(p.Pts())
	p.SetDuration(durUS * den / 1_000_000)
	return w.fc.WriteInterleavedFrame(p)
}

// truncateFile keeps only fraction of the file, cutting mid-record.
func truncateFile(path string, fraction float64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	keep := int64(float64(fi.Size()) * fraction)
	if keep < 1024 {
		keep = 1024
	}
	if keep >= fi.Size() {
		return nil
	}
	if err := os.Truncate(path, keep); err != nil {
		return fmt.Errorf("fixture: truncate: %w", err)
	}
	return nil
}
