// Package demux turns a byte-stream input into access units incrementally.
// Reads go through a fixed-size chunk reader so pipeline memory stays bounded
// by the configured chunk budget; parser buffering itself never exceeds one
// NAL unit plus one read window (see internal/codec).
package demux

import (
	"io"

	"mediapack/internal/codec"
)

// ChunkReader caps every Read at Chunk bytes and tracks total bytes read.
type ChunkReader struct {
	r         io.Reader
	Chunk     int
	BytesRead int64
}

// NewChunkReader wraps r with the given per-read byte budget.
func NewChunkReader(r io.Reader, chunk int) *ChunkReader {
	return &ChunkReader{r: r, Chunk: chunk}
}

// Read fills up to Chunk bytes.
func (c *ChunkReader) Read(p []byte) (int, error) {
	if len(p) > c.Chunk {
		p = p[:c.Chunk]
	}
	n, err := c.r.Read(p)
	c.BytesRead += int64(n)
	return n, err
}

// Demuxer is the incremental access-unit reader for one elementary stream.
type Demuxer struct {
	stream []byte
	pos    int
	track  uint32
	aus    []codec.AccessUnit
	meta   *codec.StreamMeta
}

// New parses the (already fully read, size-limited) stream and prepares the
// incremental cursor. The parse cost is paid once; resume reuses cached parse
// results keyed by input digest in higher layers.
func New(stream []byte, trackID uint32) (*Demuxer, error) {
	aus, meta, err := codec.ParseBytes(stream, trackID)
	if err != nil {
		return nil, err
	}
	return &Demuxer{stream: stream, track: trackID, aus: aus, meta: meta}, nil
}

// Next returns the next access unit, or io.EOF when the timeline is exhausted.
func (d *Demuxer) Next() (*codec.AccessUnit, error) {
	if d.pos >= len(d.aus) {
		return nil, io.EOF
	}
	au := d.aus[d.pos]
	d.pos++
	return &au, nil
}

// Remaining reports how many access units have not yet been emitted.
func (d *Demuxer) Remaining() int { return len(d.aus) - d.pos }

// Position returns the number of access units emitted so far.
func (d *Demuxer) Position() int { return d.pos }

// Meta returns the parsed stream metadata.
func (d *Demuxer) Meta() *codec.StreamMeta { return d.meta }

// AccessUnits exposes the full parsed timeline for snapshotting.
func (d *Demuxer) AccessUnits() []codec.AccessUnit { return d.aus }
