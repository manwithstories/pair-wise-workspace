// Package demux reads ISO BMFF containers into compressed samples expressed in
// the unified 90 kHz timebase. Structural walking uses internal/mp4box for
// exact offset accounting while go-mp4 unmarshals sample-table payloads.
package demux

import (
	"io"

	mp4 "github.com/abema/go-mp4"

	"mediacore/internal/fault"
	"mediacore/internal/mp4box"
	"mediacore/internal/sample"
)

// Limits bounds every allocation and read so malformed inputs cannot trigger
// unbounded memory use.
type Limits struct {
	MaxFileBytes   int64
	MaxBoxPayload  int
	MaxTracks      int
	MaxSamples     int
	MaxSampleBytes int
}

// DefaultLimits are the demo acceptance limits.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes:   256 << 20,
		MaxBoxPayload:  16 << 20,
		MaxTracks:      32,
		MaxSamples:     200_000,
		MaxSampleBytes: 4 << 20,
	}
}

// TrackMeta describes one parsed source track before sample reading.
type TrackMeta struct {
	Index     int
	TrackID   uint32
	Codec     sample.Codec
	Timescale uint32
	Handler   string
	Samples   int
	Keyframes map[int]bool
	// chunkOff/sizes are the resolved per-sample absolute offsets.
	chunkOff []int64
	sizes    []uint32
	sttsRuns []sttsRun
}

type sttsRun struct {
	count uint32
	delta uint32
}

// Reader parses a seekable container.
type Reader struct {
	rs          io.ReadSeeker
	lim         Limits
	fileSz      int64
	mdatPayload int64
	mdatEnd     int64
	missing     map[int]bool
}

// NewReader opens a container and determines its size.
func NewReader(rs io.ReadSeeker, lim Limits) (*Reader, error) {
	size, err := rs.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	if size > lim.MaxFileBytes {
		return nil, fault.New(fault.KindLimitExceeded, fault.Abort, 0,
			"file exceeds configured maximum size")
	}
	return &Reader{rs: rs, lim: lim, fileSz: size, missing: map[int]bool{}}, nil
}

// TopLevel scans the root boxes and returns their headers.
func (r *Reader) TopLevel() ([]*mp4box.Header, error) {
	return mp4box.Children(r.rs, &mp4box.Header{PayloadOff: 0, End: r.fileSz}, r.fileSz)
}

// unmarshal reads a box payload and decodes it through go-mp4.
func (r *Reader) unmarshal(h *mp4box.Header, dst mp4.IBox) error {
	data, err := mp4box.Payload(r.rs, h, r.lim.MaxBoxPayload)
	if err != nil {
		return err
	}
	if _, err := mp4.Unmarshal(bytesReader(data), uint64(len(data)), dst, mp4.Context{}); err != nil {
		return fault.New(fault.KindCorruptSample, fault.Abort, h.PayloadOff,
			"box payload unmarshal failed: "+err.Error())
	}
	return nil
}
