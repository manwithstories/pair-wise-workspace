// Package remux writes a lossless MP4 container around Matroska sample data.
// Sample payloads are copied verbatim from the source byte ranges recorded by
// the demuxer; the package never transcodes. Unsupported codecs are refused
// before any output is created.
package remux

import (
	"errors"
	"fmt"
	"io"
	"sort"

	"archivecheck.local/archivecheck/internal/clock"
	"archivecheck.local/archivecheck/internal/fault"
	"archivecheck.local/archivecheck/internal/track"
)

// Sample is one frame to write, already mapped to output timestamps.
//
// Exactly one payload source is used: Data when it is non-nil, otherwise
// Reader with Offset and Size. The latter lets the pipeline stream frames
// from a spool file so no stage ever holds the whole media in memory.
type Sample struct {
	TrackNumber uint64
	// Data is the exact source bytes for this frame.
	Data []byte
	// Reader, Offset and Size describe the payload when Data is nil.
	Reader io.ReaderAt
	Offset int64
	Size   int64
	// SourceOffset is where the frame came from in the Matroska input; it is
	// recorded in the manifest so a reviewer can re-read the master.
	SourceOffset int64
	DTS          int64
	PTS          int64
	Duration     int64
	Keyframe     bool
	// StaticDuration marks tracks whose samples all share DefaultDuration.
	StaticDuration bool
}

// TrackPlan is a validated track plus its side data.
type TrackPlan struct {
	Track track.Track
	// Width and Height are in pixels; zero for non-video tracks.
	Width  int
	Height int
	// SampleRate and Channels describe audio tracks.
	SampleRate int
	Channels   int
	// Language is an ISO-639-2 code; "und" when unknown.
	Language string
	// Handler is "vide", "soun" or "sbtl".
	Handler string
	// Samples are ordered by DTS; the writer emits them in this order.
	Samples []Sample
	// Timescale is the MP4 media timescale.
	Timescale int64
	// TimescaleMovie is the movie timescale used for tkhd/elst durations.
	TimescaleMovie int64
	// Shift is added as an empty edit when positive, preserving presentation
	// timing for sources whose first DTS was not zero.
	Shift int64
}

// Plan is the complete input to the writer.
type Plan struct {
	Tracks []*TrackPlan
	// MovieTimescale is used by mvhd and edit lists.
	MovieTimescale int64
	// Brand is the major ftyp brand, e.g. "isom".
	Brand string
}

// Summary reports the produced output for verification and terminal output.
type Summary struct {
	// TrackSamples counts written samples per Matroska track number.
	TrackSamples map[uint64]int64
	// ChunkOffsets records the absolute output offset of each track chunk.
	ChunkOffsets map[uint64][]int64
	// SampleSizes records each sample's byte length in output order.
	SampleSizes map[uint64][]int32
	// SampleOffsets records each sample's absolute output offset.
	SampleOffsets map[uint64][]int64
	// Keyframes lists 1-based sample numbers per track.
	Keyframes map[uint64][]uint32
	// Size is the total file size written.
	Size int64
	// MdatOffset is the absolute offset of the mdat payload.
	MdatOffset int64
	// Manifest is one line per sample, used by the verifier.
	Manifest []ManifestEntry
}

// ManifestEntry ties an output sample to its source range.
type ManifestEntry struct {
	Track        uint64 `json:"track"`
	OutputSample uint32 `json:"output_sample"`
	SourceOffset int64  `json:"source_offset"`
	Size         int    `json:"size"`
	Keyframe     bool   `json:"keyframe"`
	// SHA256 is filled by the verifier after re-reading the output.
	SHA256 string `json:"sha256,omitempty"`
}

// ErrNothingToWrite is returned for a plan with no samples.
var ErrNothingToWrite = errors.New("remux: no samples to write")

// Validate checks the plan before any bytes are written.
func (p *Plan) Validate() error {
	if len(p.Tracks) == 0 {
		return fault.New(fault.StageRemux, fmt.Errorf("%w: no tracks", fault.ErrMalformed))
	}
	for _, t := range p.Tracks {
		if t == nil {
			return fault.New(fault.StageRemux, fault.ErrMalformed)
		}
		switch t.Track.Mapping.Kind {
		case track.KindAVC, track.KindHEVC, track.KindVP9, track.KindAV1:
			if t.Width <= 0 || t.Height <= 0 {
				return fault.New(fault.StageRemux,
					fmt.Errorf("%w: video track %d has no dimensions", fault.ErrMalformed, t.Track.Number)).OnTrack(t.Track.Number)
			}
		case track.KindAAC, track.KindOpus, track.KindFLAC:
			if t.SampleRate <= 0 || t.Channels <= 0 {
				return fault.New(fault.StageRemux,
					fmt.Errorf("%w: audio track %d has no rate/channels", fault.ErrMalformed, t.Track.Number)).OnTrack(t.Track.Number)
			}
		case track.KindText, track.KindUnsupported:
			if t.Track.Mapping.Kind == track.KindUnsupported {
				return fault.New(fault.StageRemux, fault.ErrUnsupportedCodec).OnTrack(t.Track.Number)
			}
		}
		if t.Timescale <= 0 || t.TimescaleMovie <= 0 {
			return fault.New(fault.StageRemux, clock.ErrInvalidTimescale).OnTrack(t.Track.Number)
		}
		prev := int64(-1)
		for _, s := range t.Samples {
			if s.DTS < prev {
				return fault.New(fault.StageRemux,
					fmt.Errorf("%w: samples not in decode order", fault.ErrMalformed)).OnTrack(t.Track.Number)
			}
			prev = s.DTS
			if sampleSize(s) <= 0 {
				return fault.New(fault.StageRemux, fault.ErrCorruptBlock).OnTrack(t.Track.Number)
			}
		}
		if len(t.Samples) == 0 {
			// An empty track is legal but useless; refuse so output cannot be
			// mistaken for a faithful copy.
			return fault.New(fault.StageRemux,
				fmt.Errorf("%w: track %d has no samples", fault.ErrMalformed, t.Track.Number)).OnTrack(t.Track.Number)
		}
	}
	return nil
}

// DefaultMovieTimescale is the movie timescale used when a plan does not set
// one; 1000 keeps edit-list durations in milliseconds.
const DefaultMovieTimescale int64 = 1000

// SortTracks orders tracks by destination ID for deterministic output.
func SortTracks(tracks []*TrackPlan) {
	sort.SliceStable(tracks, func(i, j int) bool {
		return tracks[i].Track.DestTrackID < tracks[j].Track.DestTrackID
	})
}

// chunkPlan groups consecutive samples into chunks. Each chunk is written as
// one contiguous mdat run so stco/stsc stay small; a chunk boundary is forced
// whenever a sample's source range is not contiguous with the previous one.
type chunkPlan struct {
	firstSample int
	count       int
}

// planChunks groups samples into fixed-size chunks bounded by chunkSize.
func planChunks(n int, chunkSize int) []chunkPlan {
	if chunkSize <= 0 {
		chunkSize = 1
	}
	var out []chunkPlan
	for i := 0; i < n; i += chunkSize {
		end := i + chunkSize
		if end > n {
			end = n
		}
		out = append(out, chunkPlan{firstSample: i, count: end - i})
	}
	return out
}

// sampleSize returns the payload length.
func sampleSize(s Sample) int64 {
	if s.Data != nil {
		return int64(len(s.Data))
	}
	return s.Size
}

// copySample writes a sample's bytes to w, refusing short writes.
func copySample(w io.Writer, s Sample) (int64, error) {
	if s.Data != nil {
		n, err := w.Write(s.Data)
		if err != nil {
			return int64(n), fault.New(fault.StageRemux, fmt.Errorf("%w: %v", fault.ErrOutput, err))
		}
		if n != len(s.Data) {
			return int64(n), fault.New(fault.StageRemux,
				fmt.Errorf("%w: short write %d of %d", fault.ErrOutput, n, len(s.Data)))
		}
		return int64(n), nil
	}
	if s.Reader == nil {
		return 0, fault.New(fault.StageRemux, fmt.Errorf("%w: no payload source", fault.ErrOutput))
	}
	sr := io.NewSectionReader(s.Reader, s.Offset, s.Size)
	n, err := io.Copy(w, sr)
	if err != nil {
		return n, fault.New(fault.StageRemux, fmt.Errorf("%w: %v", fault.ErrOutput, err))
	}
	if n != s.Size {
		return n, fault.New(fault.StageRemux,
			fmt.Errorf("%w: short copy %d of %d", fault.ErrOutput, n, s.Size))
	}
	return n, nil
}
