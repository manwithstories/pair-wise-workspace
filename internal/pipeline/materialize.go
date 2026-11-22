package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"example.com/medialab/archive/internal/codec"
	"example.com/medialab/archive/internal/model"
	"example.com/medialab/archive/internal/mp4io"
	"example.com/medialab/archive/internal/segment"
)

// ArtefactDir returns the per-run output directory, always inside WorkDir.
func (r *Runner) ArtefactDir() string {
	return filepath.Join(r.cfg.WorkDir, fmt.Sprintf("seed-%d", r.cfg.Seed))
}

// WriteInitSegments writes one ftyp+moov init segment per representation and
// returns the sha256 per representation. Init segments are identical for every
// run of the same configuration, which is what makes them safe to cache.
func (r *Runner) WriteInitSegments() (map[model.RepresentationID]string, error) {
	dir := filepath.Join(r.ArtefactDir(), "init")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := map[model.RepresentationID]string{}
	for _, st := range r.streams {
		cfg := mp4io.Config{
			Rep: st.spec.rep.ID, Codec: st.spec.rep.Codec,
			SampleRate: int(st.spec.opts.ClockRate), Channels: 2, Bitrate: st.spec.rep.Bitrate,
		}
		if st.spec.rep.Codec == model.CodecAAC {
			cfg.SampleRate = st.spec.opts.AAC.SampleRate
		}
		data, err := cfg.InitSegment()
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, string(st.spec.rep.ID)+".mp4"), data, 0o644); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		out[st.spec.rep.ID] = hex.EncodeToString(sum[:])
	}
	return out, nil
}

// materialize writes one media segment and verifies it by re-parsing. It is
// installed as the runner's materializer for demo runs.
func (r *Runner) materializeSegment(st *streamState, e *segment.Entry) (Materialized, error) {
	dir := filepath.Join(r.ArtefactDir(), "media", string(st.spec.rep.ID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Materialized{}, err
	}
	samples, err := r.samplesFor(st, e.StartUS, e.EndUS)
	if err != nil {
		return Materialized{}, err
	}
	if len(samples) == 0 {
		// A window with no usable packet is still delivered as an explicitly
		// silent sample so the timeline keeps its length rather than vanishing.
		payload, err := silencePayload(st)
		if err != nil {
			return Materialized{}, err
		}
		samples = []mp4io.Sample{{DurationUS: e.EndUS - e.StartUS, Payload: payload}}
	}
	media, err := mp4io.MediaSegment(samples, e.StartUS, uint32(e.Index))
	if err != nil {
		return Materialized{}, err
	}
	if _, err := mp4io.Verify(media); err != nil {
		return Materialized{}, fmt.Errorf("segment %s/%d v%d failed verification: %w", e.Rep, e.Index, e.Version, err)
	}
	name := fmt.Sprintf("%05d-v%d.m4s", e.Index, e.Version)
	if err := os.WriteFile(filepath.Join(dir, name), media, 0o644); err != nil {
		return Materialized{}, err
	}
	initSHA, err := r.initSHA(st)
	if err != nil {
		return Materialized{}, err
	}
	sum := sha256.Sum256(media)
	return Materialized{InitSHA256: initSHA, MediaSize: int64(len(media)), MediaSHA256: hex.EncodeToString(sum[:])}, nil
}

// samplesFor converts the active-window events of one stream into MP4 samples
// clipped to the segment window. The first and last sample are trimmed to the
// window so consecutive segments neither overlap nor leave a hole.
func (r *Runner) samplesFor(st *streamState, startUS, endUS int64) ([]mp4io.Sample, error) {
	var out []mp4io.Sample
	cursor := startUS
	for _, ev := range st.events {
		evEnd := ev.ExtTimeUS + max64(ev.DurationUS, 0)
		if ev.DurationUS <= 0 || evEnd <= startUS || ev.ExtTimeUS >= endUS {
			continue
		}
		s, e := max64(ev.ExtTimeUS, startUS), min64(evEnd, endUS)
		if s > cursor {
			// A recovered or uncovered gap: labelled silence of exact length.
			payload, err := silencePayload(st)
			if err != nil {
				return nil, err
			}
			out = append(out, mp4io.Sample{DurationUS: s - cursor, Payload: payload})
		}
		payload := ev.Payload
		if len(payload) == 0 {
			var err error
			payload, err = silencePayload(st)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, mp4io.Sample{DurationUS: e - s, Payload: payload})
		cursor = e
	}
	if cursor < endUS {
		payload, err := silencePayload(st)
		if err != nil {
			return nil, err
		}
		out = append(out, mp4io.Sample{DurationUS: endUS - cursor, Payload: payload})
	}
	return out, nil
}

func silencePayload(st *streamState) ([]byte, error) {
	if st.spec.rep.Codec == model.CodecAAC {
		return st.spec.opts.AAC.SilenceAACPayload()
	}
	return codec.SilenceOpusPayload(st.spec.rep.FrameDurationUS)
}

func (r *Runner) initSHA(st *streamState) (string, error) {
	path := filepath.Join(r.ArtefactDir(), "init", string(st.spec.rep.ID)+".mp4")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// EnableMaterialization writes init segments and installs the media writer.
func (r *Runner) EnableMaterialization() error {
	initSHA, err := r.WriteInitSegments()
	if err != nil {
		return err
	}
	r.initHashes = initSHA
	r.materialize = r.materializeSegment
	return nil
}
