package scan

import (
	"crypto/sha256"
	"encoding/binary"
	"io"

	"medpipe/internal/model"
)

// FrameIndexEntry locates one video record inside the container.
type FrameIndexEntry struct {
	Seq       uint32 `json:"seq"`
	Offset    int64  `json:"offset"`
	StartNS   int64  `json:"start_ns"`
	EndNS     int64  `json:"end_ns"`
	Keyframe  bool   `json:"keyframe"`
	Digest    string `json:"digest"`
}

// ScanIndex is the digest-bound summary produced after a successful scan.
// The store layer refuses any index whose SourceDigest does not match the
// container bytes or whose required fields are missing/zero.
type ScanIndex struct {
	SchemaVersion int               `json:"schema_version"`
	SourceDigest  string            `json:"source_digest"`
	SourceSize    int64             `json:"source_size"`
	FrameCount    int               `json:"frame_count"`
	AudioCount    int               `json:"audio_count"`
	TimeMinNS     int64             `json:"time_min_ns"`
	TimeMaxNS     int64             `json:"time_max_ns"`
	Frames        []FrameIndexEntry `json:"frames"`
	AudioOffsets  []int64           `json:"audio_offsets"`
}

// DigestSource computes the SHA-256 over the exact container bytes that the
// index describes, so later tampering invalidates the index automatically.
func DigestSource(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	sum := h.Sum(nil)
	return encodeHex(sum), n, nil
}

func encodeHex(b []byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[2*i] = hexChars[v>>4]
		out[2*i+1] = hexChars[v&0x0f]
	}
	return string(out)
}

// BuildIndex turns a scan result into a summary index bound to sourceDigest.
// EndNS is estimated from the next frame's timestamp; the final frame reuses
// its own duration when a successor is unavailable.
func BuildIndex(res *Result, sourceDigest string, sourceSize int64) *ScanIndex {
	idx := &ScanIndex{
		SchemaVersion: 1,
		SourceDigest:  sourceDigest,
		SourceSize:    sourceSize,
		FrameCount:    len(res.Frames),
		AudioCount:    len(res.Audio),
		Frames:        make([]FrameIndexEntry, 0, len(res.Frames)),
		AudioOffsets:  make([]int64, 0, len(res.Audio)),
	}
	if len(res.Frames) > 0 {
		idx.TimeMinNS = res.Frames[0].TimestampNS
		idx.TimeMaxNS = res.Frames[len(res.Frames)-1].TimestampNS
	}
	for i, f := range res.Frames {
		end := f.TimestampNS
		if i+1 < len(res.Frames) {
			end = res.Frames[i+1].TimestampNS
		}
		idx.Frames = append(idx.Frames, FrameIndexEntry{
			Seq: f.Seq, Offset: f.Offset, StartNS: f.TimestampNS, EndNS: end,
			Keyframe: f.Keyframe, Digest: encodeHex(f.Checksum[:]),
		})
	}
	for _, a := range res.Audio {
		idx.AudioOffsets = append(idx.AudioOffsets, a.Offset)
		if a.TimestampNS < idx.TimeMinNS || idx.FrameCount == 0 {
			idx.TimeMinNS = min64(idx.TimeMinNS, a.TimestampNS)
		}
	}
	return idx
}

// Validate enforces required fields so a partially written or hand-edited
// index cannot be mistaken for a valid one.
func (idx *ScanIndex) Validate() error {
	if idx == nil {
		return model.NewError(model.ErrIndexField, "index is nil")
	}
	if idx.SchemaVersion != 1 {
		return model.NewError(model.ErrIndexField, "schema_version=%d", idx.SchemaVersion)
	}
	if len(idx.SourceDigest) != sha256.Size*2 || idx.SourceSize <= 0 {
		return model.NewError(model.ErrIndexIncomplete, "digest or size missing")
	}
	if idx.FrameCount != len(idx.Frames) {
		return model.NewError(model.ErrIndexIncomplete, "frame_count mismatch: %d vs %d", idx.FrameCount, len(idx.Frames))
	}
	if idx.AudioCount != len(idx.AudioOffsets) {
		return model.NewError(model.ErrIndexIncomplete, "audio_count mismatch")
	}
	for i, e := range idx.Frames {
		if e.Offset < 0 || len(e.Digest) != sha256.Size*2 || e.EndNS < e.StartNS {
			return model.NewError(model.ErrIndexField, "frame entry %d incomplete", i)
		}
	}
	return nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Uint32At is a small shared helper used by corruption fixtures.
func Uint32At(b []byte, off int) uint32 {
	return binary.BigEndian.Uint32(b[off : off+4])
}
