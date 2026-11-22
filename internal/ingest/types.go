// Package ingest streams container inputs (a documented MP4 box subset and
// a generated MXF OP1a subset) into one uniform sample-event stream. Parsers
// walk byte boundaries themselves so truncation and out-of-bounds structures
// are reported with the precise input offset instead of being skipped.
package ingest

import (
	"io"

	"mediarun/internal/timebase"
)

// MaxBlockBytes is the hard ceiling for a single sample block/chunk
// (specification: 4 MiB). A sample table advertising a larger sample is a
// BLOCK_TOO_LARGE error, never an oversized allocation.
const MaxBlockBytes = 4 * 1024 * 1024

// TrackKind identifies the media family of a track.
type TrackKind string

const (
	KindVideo TrackKind = "video"
	KindAudio TrackKind = "audio"
)

// TrackDesc describes one track discovered in the container header.
type TrackDesc struct {
	ID       string    // stable identifier, e.g. "video/0", "audio/0"
	Kind     TrackKind // video | audio
	Codec    string    // fourCC / MXF label, e.g. "avc1", "mp4a"
	Timescale int64    // ticks per second (MP4 mdhd timescale; MXF edit rate)
	Width    uint32    // coded width in pixels (video)
	Height   uint32    // coded height in pixels (video)
	Channels uint16    // channel count (audio)
	SampleCount int64  // total samples advertised by the header tables
}

// SampleEvent is the uniform unit exchanged between ingest and the
// pipeline. Payload is streamed: the caller owns the reader and must fully
// consume (or discard) it before pulling the next event.
type SampleEvent struct {
	TrackID      string
	Index        int64             // zero-based sample index within the track
	DataOffset   int64             // absolute byte offset of payload in the input
	Size         int64             // payload size in bytes
	DTS          int64             // decode timestamp in track ticks
	PTS          int64             // presentation timestamp in track ticks
	Duration     int64             // sample duration in track ticks
	Timebase     timebase.Timebase // track clock
	Payload      io.Reader         // streamed payload (Size bytes), nil in header-only passes
}

// ContainerInfo summarizes one parsed input header.
type ContainerInfo struct {
	Format     string       // "mp4" | "mxf"
	Path       string
	Size       int64        // input file size in bytes
	Tracks     []TrackDesc
	InputHash  string       // hex SHA-256 of the whole input, filled by the runner
}

// Streamer is implemented by every container parser.
type Streamer interface {
	// Info returns the parsed container header (track list, geometry).
	Info() ContainerInfo
	// Next returns the next sample event in interleaved file order, or
	// io.EOF when every advertised sample has been emitted.
	Next() (*SampleEvent, error)
	// Close releases underlying file handles.
	Close() error
}
