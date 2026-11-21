// Package summarize builds the output description required by the acceptance
// criteria: track mapping, sample counts, time range and input/output SHA-256,
// plus per-track compressed-sample digests.
package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"mediamerge/internal/checksum"
	"mediamerge/internal/clock"
	"mediamerge/internal/demux"
	"mediamerge/internal/mapping"
	"mediamerge/internal/model"
)

// TrackSummary is one line of the machine-readable summary.
type TrackSummary struct {
	InputID    uint32 `json:"input_track"`
	TargetID   uint32 `json:"target_track"`
	Media      string `json:"media"`
	Codec      string `json:"codec"`
	Timescale  uint32 `json:"timescale"`
	Samples    int    `json:"samples"`
	FirstMicro uint64 `json:"first_us"`
	LastMicro  uint64 `json:"last_us"`
	InputHash  string `json:"sample_sha256_in"`
	OutputHash string `json:"sample_sha256_out"`
	Match      bool   `json:"match"`
}

// Summary is the complete deterministic output description.
type Summary struct {
	Tracks     int            `json:"tracks"`
	Samples    int            `json:"samples"`
	InputSHA   string         `json:"input_sha256"`
	OutputSHA  string         `json:"output_sha256"`
	SampleSame bool           `json:"sample_sha256_match"`
	Track      []TrackSummary `json:"track_summary"`
	Timebase   string         `json:"timebase_note"`
}

// Text renders the stable one-line-per-track summary.
func (s *Summary) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "tracks=%d samples=%d input_sha256=%s output_sha256=%s sample_sha256_match=%t\n",
		s.Tracks, s.Samples, s.InputSHA, s.OutputSHA, s.SampleSame)
	for _, t := range s.Track {
		fmt.Fprintf(&b, "track input=%d target=%d media=%s codec=%s timescale=%d samples=%d "+
			"first_us=%d last_us=%d sample_sha256_match=%t\n",
			t.InputID, t.TargetID, t.Media, t.Codec, t.Timescale, t.Samples,
			t.FirstMicro, t.LastMicro, t.Match)
	}
	return b.String()
}

// JSON renders the summary as indented JSON.
func (s *Summary) JSON() string {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Run reads both files and assembles the summary. It does not trust the
// journal: digests are recomputed from the bytes.
func Run(ctx context.Context, in, out *demux.Reader, dir string) (*Summary, error) {
	plan, err := mapping.Apply(in.Tracks, mapping.DefaultRules(in.Tracks))
	if err != nil {
		return nil, err
	}
	if len(out.Tracks) != len(plan.Tracks) {
		return nil, model.NewError(model.StageVerify, model.CodeIntegrity, 0, 0, false,
			"output has %d tracks, expected %d", len(out.Tracks), len(plan.Tracks))
	}
	inSHA, _, err := checksum.File(in.Path)
	if err != nil {
		return nil, err
	}
	outPath := out.Path
	outSHA, outLen, err := checksum.File(outPath)
	if err != nil {
		return nil, err
	}
	s := &Summary{Tracks: len(plan.Tracks), InputSHA: inSHA, OutputSHA: outSHA,
		SampleSame: true, Timebase: "integer microseconds; exact rational conversion, no float accumulation"}
	lastEnd := make(map[int]uint64, len(plan.Tracks))
	for ti, tr := range plan.Tracks {
		if tr.Timescale == 0 {
			return nil, model.NewError(model.StageVerify, model.CodeTimebase, -1, tr.InputID, false,
				"track %d has zero timescale", tr.InputID)
		}
		first, err := clock.Micros(tr.FirstDTS, tr.Timescale)
		if err != nil {
			return nil, err
		}
		lastEnd[ti] = tr.LastDTS + uint64(durationOf(tr))
		last, err := clock.Micros(lastEnd[ti], tr.Timescale)
		if err != nil {
			return nil, err
		}
		inHash, err := sampleDigest(in, tr.Samples)
		if err != nil {
			return nil, err
		}
		outHash, err := sampleDigest(out, out.Tracks[ti].Samples)
		if err != nil {
			return nil, err
		}
		match := inHash == outHash && len(tr.Samples) == len(out.Tracks[ti].Samples)
		if !match {
			s.SampleSame = false
		}
		s.Track = append(s.Track, TrackSummary{
			InputID: tr.InputID, TargetID: tr.TargetID, Media: string(tr.MediaType), Codec: tr.Codec,
			Timescale: tr.Timescale, Samples: len(tr.Samples), FirstMicro: first, LastMicro: last,
			InputHash: inHash, OutputHash: outHash, Match: match,
		})
		s.Samples += len(tr.Samples)
	}
	_ = dir
	_ = outLen
	return s, nil
}

func durationOf(t *demux.Track) uint32 {
	if len(t.Samples) == 0 {
		return 0
	}
	return t.Samples[len(t.Samples)-1].Duration
}

func sampleDigest(r *demux.Reader, refs []model.SampleRef) (string, error) {
	h := checksum.NewSampleHasher()
	for _, ref := range refs {
		data, err := r.ReadSample(ref)
		if err != nil {
			return "", err
		}
		h.Add(data)
	}
	return h.Sum(), nil
}
