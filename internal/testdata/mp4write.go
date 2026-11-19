// Package testdata generates deterministic fixtures for every mediasync
// stage: subtitle files (valid and damaged), MP4 files with injected index
// faults, RTP packet streams with reorder/dup/loss and PCM sample blocks.
//
// Nothing here reads external media: every byte is produced by this package.
package testdata

import (
	"encoding/binary"
	"fmt"

	"github.com/abema/go-mp4"
)

// MP4Spec describes one generated MP4 file.
type MP4Spec struct {
	Timescale   uint32
	SampleCount int
	SampleDelta uint32
	KeyInterval int
	SampleSize  int
	PayloadSeed int64
	// PaddingBytes inserts a free box before mdat so identical payloads can
	// live at different absolute offsets on different cameras.
	PaddingBytes int
	// Fault selects an injected index fault, see the Fault* constants.
	Fault Fault
}

// Fault enumerates the index damage variants used by tests.
type Fault string

const (
	FaultNone           Fault = ""
	FaultChunkOutOfFile Fault = "chunk_offset_out_of_file"
	FaultStssOutOfRange Fault = "stss_sample_out_of_range"
	FaultNoStss         Fault = "stss_missing"
	FaultZeroDelta      Fault = "stts_zero_delta"
	FaultTruncatedMoov  Fault = "truncated_moov"
)

// DefaultSpec returns a 25 fps, 8 s clip with a keyframe every second.
func DefaultSpec(seed int64) MP4Spec {
	return MP4Spec{Timescale: 12800, SampleCount: 200, SampleDelta: 512, KeyInterval: 25,
		SampleSize: 64, PayloadSeed: seed}
}

// AltSpec returns a 29.97 fps clip in a distinct clock domain.
func AltSpec(seed int64) MP4Spec {
	return MP4Spec{Timescale: 30000, SampleCount: 240, SampleDelta: 1001, KeyInterval: 30,
		SampleSize: 64, PayloadSeed: seed}
}

// payload produces deterministic sample bytes. Identical seeds give
// byte-identical payloads regardless of where the samples are stored.
func payload(seed int64, index, size int) []byte {
	out := make([]byte, size)
	x := uint64(seed)*6364136223846793005 + uint64(index+1)*1442695040888963407
	for i := range out {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		out[i] = byte(x >> 33)
	}
	return out
}

type memFile struct {
	buf []byte
	off int64
}

func (m *memFile) Write(p []byte) (int, error) {
	need := int(m.off) + len(p)
	if need > len(m.buf) {
		m.buf = append(m.buf, make([]byte, need-len(m.buf))...)
	}
	copy(m.buf[m.off:], p)
	m.off += int64(len(p))
	return len(p), nil
}

func (m *memFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		m.off = offset
	case 1:
		m.off += offset
	case 2:
		m.off = int64(len(m.buf)) + offset
	}
	return m.off, nil
}

func start(w *mp4.Writer, t mp4.BoxType) *mp4.BoxInfo {
	bi, err := w.StartBox(&mp4.BoxInfo{Type: t})
	if err != nil {
		panic(fmt.Sprintf("testdata: start box %s: %v", t, err))
	}
	return bi
}

func leaf(w *mp4.Writer, box mp4.IImmutableBox) {
	if _, err := mp4.Marshal(w, box, mp4.Context{}); err != nil {
		panic(fmt.Sprintf("testdata: marshal box %s: %v", box.GetType(), err))
	}
}

func end(w *mp4.Writer) *mp4.BoxInfo {
	bi, err := w.EndBox()
	if err != nil {
		panic(fmt.Sprintf("testdata: end box: %v", err))
	}
	return bi
}

// findBox returns the offset of the first top-level box with the given type.
func findBox(data []byte, want [4]byte) int {
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off:]))
		typ := data[off+4 : off+8]
		if typ[0] == want[0] && typ[1] == want[1] && typ[2] == want[2] && typ[3] == want[3] {
			return off
		}
		if size < 8 {
			return -1
		}
		off += size
	}
	return -1
}
