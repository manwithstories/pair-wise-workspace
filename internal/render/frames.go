// Package render generates the deterministic off-screen frame raster
// sequence from a composed timeline. No real video decoding happens:
// each frame's pixels are a deterministic function of its source
// identity, so identical inputs always yield identical SHA-256 hashes.
package render

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	"mediatimeline/internal/timeline"
)

// Fixed raster geometry for every output frame.
const (
	Width  = 48
	Height = 27
	Bytes  = Width * Height * 3
)

// Raster produces the deterministic 48x27 RGB raster for a frame. The
// pixel content is derived from a SHA-256 seed via a xorshift64*
// generator — cheap, exact and platform independent.
func Raster(trackID string, seq int, srcOffset, refStart int64) []byte {
	seed := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d", trackID, seq, srcOffset, refStart)))
	state := binary.BigEndian.Uint64(seed[:8]) | 1 // never zero
	out := make([]byte, Bytes)
	for i := 0; i < len(out); i += 8 {
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		x := state * 2685821657736338717
		binary.BigEndian.PutUint64(out[i:], x)
	}
	return out
}

// FrameOut is the manifest record for one rendered frame.
type FrameOut struct {
	Index     int    `json:"index"`
	SHA256    string `json:"sha256"`
	Track     string `json:"track"`
	SampleSeq int    `json:"sample_seq"`
	SrcOffset int64  `json:"src_offset"`
	RefStart  int64  `json:"ref_start"`
	RefEnd    int64  `json:"ref_end"`
}

// Summary computes the aggregate digest over per-frame hashes.
func Summary(frames []FrameOut) string {
	h := sha256.New()
	for _, f := range frames {
		raw, _ := hex.DecodeString(f.SHA256)
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Render writes the rasters of all frames to w (concatenated, in
// order) and returns the per-frame manifest records.
func Render(frames []timeline.Frame, w io.Writer) ([]FrameOut, error) {
	out := make([]FrameOut, 0, len(frames))
	for _, f := range frames {
		raster := Raster(f.TrackID, f.SampleSeq, f.SrcOffset, f.RefStart)
		if _, err := w.Write(raster); err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raster)
		out = append(out, FrameOut{
			Index: f.Index, SHA256: hex.EncodeToString(sum[:]),
			Track: f.TrackID, SampleSeq: f.SampleSeq, SrcOffset: f.SrcOffset,
			RefStart: f.RefStart, RefEnd: f.RefEnd,
		})
	}
	return out, nil
}
