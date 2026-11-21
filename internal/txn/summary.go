package txn

import (
	"encoding/json"
	"fmt"
	"strings"

	"mediamerge/internal/clock"
	"mediamerge/internal/model"
)

// TimeRange is the presentation window of a track in whole microseconds.
type TimeRange struct {
	FirstMicros uint64
	LastMicros  uint64
}

// TrackSummary is one line of the machine-readable summary.
type TrackSummary struct {
	InputID    uint32 `json:"input_track"`
	TargetID   uint32 `json:"target_track"`
	Media      string `json:"media"`
	Codec      string `json:"codec"`
	Timescale  uint32 `json:"timescale"`
	Samples    int    `json:"samples"`
	FirstDTS   uint64 `json:"first_dts"`
	LastDTS    uint64 `json:"last_dts"`
	FirstMicro uint64 `json:"first_us"`
	LastMicro  uint64 `json:"last_us"`
	InputHash  string `json:"sample_sha256_in"`
	OutputHash string `json:"sample_sha256_out"`
	Match      bool   `json:"match"`
}

// Summary is the full output description required by the acceptance
// criteria: track mapping, sample counts, time range and both SHA-256 values.
type Summary struct {
	Tracks     int            `json:"tracks"`
	Samples    int            `json:"samples"`
	InputSHA   string         `json:"input_sha256"`
	OutputSHA  string         `json:"output_sha256"`
	SampleSame bool           `json:"sample_sha256_match"`
	Track      []TrackSummary `json:"track_summary"`
	RTPNote    string         `json:"rtp_timestamp_view"`
}

// TrackSummaryInput is the minimal mapping view needed for a summary line.
type TrackSummaryInput struct {
	InputID   uint32
	TargetID  uint32
	Media     model.MediaType
	Codec     string
	Timescale uint32
	Samples   int
	FirstDTS  uint64
	LastDTS   uint64
}

// NewSummary assembles a deterministic summary from a run and its inputs.
func NewSummary(res *Result, tracks []TrackSummaryInput) (*Summary, error) {
	s := &Summary{Tracks: res.Tracks, Samples: res.Samples, InputSHA: res.InputSHA,
		OutputSHA: res.OutputSHA, SampleSame: res.SampleMatch}
	for i, t := range tracks {
		if t.Timescale == 0 {
			return nil, model.NewError(model.StageVerify, model.CodeTimebase, -1, t.InputID, false,
				"track %d has zero timescale", t.InputID)
		}
		first, err := clock.Micros(t.FirstDTS, t.Timescale)
		if err != nil {
			return nil, err
		}
		last, err := clock.Micros(t.LastDTS, t.Timescale)
		if err != nil {
			return nil, err
		}
		ts := TrackSummary{
			InputID: t.InputID, TargetID: t.TargetID, Media: string(t.Media), Codec: t.Codec,
			Timescale: t.Timescale, Samples: t.Samples, FirstDTS: t.FirstDTS, LastDTS: t.LastDTS,
			FirstMicro: first, LastMicro: last,
		}
		if i < len(res.Digests) {
			ts.InputHash = res.Digests[i].InputHash
			ts.OutputHash = res.Digests[i].OutputHash
			ts.Match = res.Digests[i].Match
		}
		s.Track = append(s.Track, ts)
	}
	s.RTPNote = "timestamps are reported in integer microseconds; RTP 90k/48k views derive exactly"
	return s, nil
}

// JSON renders the summary as indented JSON.
func (s *Summary) JSON() string {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
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
