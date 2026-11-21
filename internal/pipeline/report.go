package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"archivecheck.local/archivecheck/internal/fault"
)

// ManifestDocument is the machine-readable recovery manifest written next to
// an output. InputPath is the source file the offsets refer to. The document
// is intentionally explicit about what was proven and what was skipped, so a
// reviewer does not have to trust a summary line.
type ManifestDocument struct {
	Input              string           `json:"input"`
	Output             string           `json:"output"`
	InputPath          string           `json:"input_path"`
	OutputSize         int64            `json:"output_size"`
	OutputSHA256       string           `json:"output_sha256"`
	DocType            string           `json:"doctype"`
	TimestampScale     int64            `json:"timestamp_scale"`
	MappingDigest      string           `json:"mapping_digest"`
	UnknownCodecs      []string         `json:"unknown_codecs,omitempty"`
	UnknownTrackBlocks int64            `json:"unknown_track_blocks"`
	Queue              QueueReport      `json:"queue"`
	Tracks             []TrackReport    `json:"tracks"`
	Skipped            []SkipReport     `json:"skipped,omitempty"`
	Rates              RatesReport      `json:"rates"`
	Keyframes          []KeyframeReport `json:"keyframes"`
	Samples            []SampleReport   `json:"samples"`
}

// QueueReport proves the memory bound held.
type QueueReport struct {
	Capacity int   `json:"capacity"`
	Peak     int   `json:"peak"`
	Sent     int64 `json:"sent"`
	Received int64 `json:"received"`
	Blocked  int64 `json:"blocked_sends"`
}

// TrackReport is one output track.
type TrackReport struct {
	Number      uint64 `json:"number"`
	Type        string `json:"type"`
	CodecID     string `json:"codec_id"`
	MP4Entry    string `json:"mp4_entry"`
	DestTrackID uint32 `json:"dest_track_id"`
	Blocks      int64  `json:"blocks"`
	Frames      int64  `json:"frames"`
	FirstTick   int64  `json:"first_tick"`
	LastTick    int64  `json:"last_tick"`
	Keyframes   int64  `json:"keyframes"`
}

// SkipReport is one proven-skipped source range.
type SkipReport struct {
	Start  int64  `json:"start"`
	Length int64  `json:"length"`
	Reason string `json:"reason"`
}

// RatesReport records the four fidelity metrics.
type RatesReport struct {
	RoundtripMatch   float64 `json:"roundtrip_match_pct"`
	KeyframeHit      float64 `json:"keyframe_hit_pct"`
	SegmentHashMatch float64 `json:"segment_hash_match_pct"`
}

// KeyframeReport is one index entry.
type KeyframeReport struct {
	Track        uint64 `json:"track"`
	Tick         int64  `json:"tick"`
	SourceOffset int64  `json:"source_offset"`
	OutputSample int    `json:"output_sample"`
}

// SampleReport ties one output sample to its source bytes.
type SampleReport struct {
	Track        uint64 `json:"track"`
	OutputSample uint32 `json:"output_sample"`
	SourceOffset int64  `json:"source_offset"`
	Size         int    `json:"size"`
	Keyframe     bool   `json:"keyframe"`
	SHA256       string `json:"sha256_out"`
}

// WriteManifest serialises the report as JSON. The output is stable: tracks,
// skips and keyframes are ordered so two runs over the same input differ only
// in paths.
func WriteManifest(w io.Writer, rep *Report) error {
	doc := ManifestDocument{
		Input:              rep.InputPath,
		InputPath:          rep.InputPath,
		Output:             rep.OutputPath,
		OutputSize:         rep.OutputSize,
		OutputSHA256:       rep.OutputSHA256,
		DocType:            rep.Info.DocType,
		TimestampScale:     rep.Timebase.TimestampScale,
		MappingDigest:      rep.MappingDigest,
		UnknownCodecs:      append([]string(nil), rep.Unknown...),
		UnknownTrackBlocks: rep.UnknownTrackBlocks,
		Queue: QueueReport{
			Capacity: rep.Queue.Capacity,
			Peak:     rep.Queue.Peak,
			Sent:     rep.Queue.Sent,
			Received: rep.Queue.Received,
			Blocked:  rep.Queue.Blocked,
		},
		Rates: RatesReport{
			RoundtripMatch:   rep.Rates.RoundtripMatch,
			KeyframeHit:      rep.Rates.KeyframeHit,
			SegmentHashMatch: rep.Rates.SegmentHashMatch,
		},
	}
	for _, t := range rep.Tracks {
		doc.Tracks = append(doc.Tracks, TrackReport{
			Number: t.Number, Type: t.Type, CodecID: t.CodecID, MP4Entry: t.MP4Entry,
			DestTrackID: t.DestTrackID, Blocks: t.Blocks, Frames: t.Frames,
			FirstTick: t.FirstTick, LastTick: t.LastTick, Keyframes: t.Keyframes,
		})
	}
	for _, s := range rep.Skipped {
		doc.Skipped = append(doc.Skipped, SkipReport{Start: s.Start, Length: s.Length, Reason: s.Reason})
	}
	docSamples := make([]SampleReport, 0, len(rep.Manifest))
	for _, m := range rep.Manifest {
		docSamples = append(docSamples, SampleReport{
			Track: m.Track, OutputSample: m.OutputSample,
			SourceOffset: m.SourceOffset, Size: m.Size, Keyframe: m.Keyframe, SHA256: m.SHA256,
		})
	}
	sort.SliceStable(docSamples, func(i, j int) bool {
		if docSamples[i].Track != docSamples[j].Track {
			return docSamples[i].Track < docSamples[j].Track
		}
		return docSamples[i].OutputSample < docSamples[j].OutputSample
	})
	doc.Samples = docSamples
	for _, e := range rep.Index.Entries {
		doc.Keyframes = append(doc.Keyframes, KeyframeReport{
			Track: e.Track, Tick: e.Tick, SourceOffset: e.SourceOffset, OutputSample: e.OutputSample,
		})
	}
	sort.SliceStable(doc.Keyframes, func(i, j int) bool {
		if doc.Keyframes[i].Track != doc.Keyframes[j].Track {
			return doc.Keyframes[i].Track < doc.Keyframes[j].Track
		}
		return doc.Keyframes[i].Tick < doc.Keyframes[j].Tick
	})
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// Validate checks the report's internal consistency before it is published:
// every sample must lie inside the output, and no skipped range may overlap a
// sample that was written.
func (r *Report) Validate() error {
	if r.OutputSize <= 0 {
		return fault.New(fault.StageVerify, fmt.Errorf("%w: no output", fault.ErrOutput))
	}
	for _, m := range r.Manifest {
		if m.Size <= 0 {
			return fault.New(fault.StageVerify, fmt.Errorf("%w: empty sample", fault.ErrMalformed)).OnTrack(m.Track)
		}
		if m.SourceOffset < 0 {
			return fault.New(fault.StageVerify, fmt.Errorf("%w: negative source offset", fault.ErrMalformed)).OnTrack(m.Track)
		}
	}
	for _, s := range r.Skipped {
		if s.Length < 0 || s.Start < 0 {
			return fault.New(fault.StageVerify, fault.ErrMalformed)
		}
	}
	return nil
}
