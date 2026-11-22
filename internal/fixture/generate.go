package fixture

import (
	"encoding/binary"
	"hash/fnv"
	"io"
	"os"

	mp4 "github.com/abema/go-mp4"
)

// TrackSpec configures one synthetic track.
type TrackSpec struct {
	Kind        string // "video" | "audio" | "markers"
	Timescale   uint32
	SampleCount uint32
	SampleDelta uint32 // per-sample duration in the track timescale
	// Audio parameters (ignored otherwise).
	Channels   uint16
	SampleRate uint32 // integer Hz, converted to 16.16 fixed point
	// Video parameters.
	Width, Height      uint16
	Primaries          uint16
	Transfer           uint16
	Matrix             uint16
	FullRange          bool
	IncludeColr        bool
	// IncludeSyncTable controls whether an stss box is written for video.
	IncludeSyncTable bool
	// KeyFrameEvery marks every nth video sample as a sync sample.
	KeyFrameEvery uint32
	// IncludeEditList attaches a synthetic elst when true.
	IncludeEditList bool
	// BadSampleLength, when > 0, overrides the stsz entry of that 0-based
	// sample with an impossibly large size.
	BadSampleLength uint32
	// BadClipEdit makes the edit list point past the track duration.
	BadClipEdit bool
}

// MP4Spec is the full description of a synthetic file.
type MP4Spec struct {
	Name   string
	Tracks []TrackSpec
	// TruncateAt, when > 0, truncates the output at this absolute byte offset.
	TruncateAt int64
}

// builtTrack holds the generated payloads and sample tables for one track.
type builtTrack struct {
	spec      *TrackSpec
	trackID   uint32
	data      []byte
	sizes     []uint32
	offsets   []uint64 // absolute file offsets, filled once mdat position known
	deltas    []mp4.SttsEntry
	sync      []uint32 // 1-based sample numbers
	chunkSize []uint32 // samples per chunk (one run per chunk)
}

// WriteMP4 renders a synthetic MP4 described by spec to path.
func WriteMP4(path string, spec MP4Spec) error {
	ms := newMemWriteSeeker()
	bw := newBoxWriter(ms)

	tracks, err := buildTracks(spec)
	if err != nil {
		return err
	}
	if err := writeFtyp(bw); err != nil {
		return err
	}
	// mdat: remember its payload start so chunk offsets can be back-filled.
	if err := bw.start(mp4.BoxTypeMdat()); err != nil {
		return err
	}
	mdatPayloadStart := uint64(ms.off)
	for _, t := range tracks {
		for i := range t.offsets {
			t.offsets[i] = mdatPayloadStart + t.offsets[i]
		}
		if _, err := bw.w.Write(t.data); err != nil {
			return err
		}
	}
	if err := bw.end(); err != nil {
		return err
	}
	if err := writeMoov(bw, spec, tracks); err != nil {
		return err
	}
	out := ms.Bytes()
	if spec.TruncateAt > 0 && int64(len(out)) > spec.TruncateAt {
		out = out[:spec.TruncateAt]
	}
	return os.WriteFile(path, out, 0o644)
}

// buildTracks renders deterministic sample payloads and table data per track.
func buildTracks(spec MP4Spec) ([]*builtTrack, error) {
	tracks := make([]*builtTrack, 0, len(spec.Tracks))
	var dataCursor uint64
	for i := range spec.Tracks {
		ts := &spec.Tracks[i]
		bt := &builtTrack{spec: ts, trackID: uint32(i + 1)}
		h := fnv.New32a()
		h.Write([]byte(spec.Name + ts.Kind))
		seed := h.Sum32()
		for s := uint32(0); s < ts.SampleCount; s++ {
			payload := samplePayload(ts.Kind, seed, s)
			bt.data = append(bt.data, payload...)
			size := uint32(len(payload))
			if ts.BadSampleLength > 0 && s == ts.BadSampleLength {
				size = 1 << 30 // impossible size relative to mdat
			}
			bt.sizes = append(bt.sizes, size)
			bt.offsets = append(bt.offsets, dataCursor)
			dataCursor += uint64(len(payload))
		}
		bt.deltas = []mp4.SttsEntry{{
			SampleCount: ts.SampleCount,
			SampleDelta: ts.SampleDelta,
		}}
		if ts.Kind == "video" && ts.IncludeSyncTable {
			step := ts.KeyFrameEvery
			if step == 0 {
				step = 12
			}
			for s := uint32(1); s <= ts.SampleCount; s++ {
				if (s-1)%step == 0 {
					bt.sync = append(bt.sync, s)
				}
			}
		}
		// One chunk per 10 samples keeps stsc/stco small but non-trivial.
		const perChunk uint32 = 10
		for remaining := ts.SampleCount; remaining > 0; {
			n := perChunk
			if remaining < perChunk {
				n = remaining
			}
			bt.chunkSize = append(bt.chunkSize, n)
			remaining -= n
		}
		tracks = append(tracks, bt)
	}
	return tracks, nil
}

// samplePayload produces a deterministic, content-free sample body. Marker
// samples are short ASCII text; media samples are fixed-size binary blobs.
func samplePayload(kind string, seed, index uint32) []byte {
	switch kind {
	case "markers":
		return []byte("MARK\n")
	case "audio":
		b := make([]byte, 32)
		binary.BigEndian.PutUint32(b, seed^index)
		return b
	default: // video
		b := make([]byte, 48)
		binary.BigEndian.PutUint32(b, seed)
		binary.BigEndian.PutUint32(b[4:], index)
		return b
	}
}

// memWriteSeeker is an in-memory io.WriteSeeker supporting backward seeks so
// go-mp4 can patch box size fields in place. A sparse byte map compacted on
// Bytes() keeps the fixture writer allocation bounded by the output size.
type memWriteSeeker struct {
	pages map[int64]byte
	off   int64
	max   int64
}

func newMemWriteSeeker() *memWriteSeeker {
	return &memWriteSeeker{pages: make(map[int64]byte)}
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
	for i, b := range p {
		m.pages[m.off+int64(i)] = b
	}
	m.off += int64(len(p))
	if m.off > m.max {
		m.max = m.off
	}
	return len(p), nil
}

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.off = offset
	case io.SeekCurrent:
		m.off += offset
	case io.SeekEnd:
		m.off = m.max + offset
	}
	return m.off, nil
}

// Bytes returns the compacted byte image. Box writers fill every byte from
// zero to max, so the map is dense by the time this is called.
func (m *memWriteSeeker) Bytes() []byte {
	out := make([]byte, m.max)
	for off, b := range m.pages {
		if off < m.max {
			out[off] = b
		}
	}
	return out
}
