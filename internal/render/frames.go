// Package render exports a deterministic offscreen review-frame sequence
// per block. Every frame file records its frame number, media time, source
// offset and SHA-256, so identical inputs always yield identical hashes and
// every output frame traces back to its source segment.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"review/internal/clock"
	"review/internal/safepath"
	"review/internal/timeline"
)

// FrameRec is the trace record of one exported frame.
type FrameRec struct {
	Frame        int64  `json:"frame"`
	MediaTime    string `json:"media_time"`
	SourceOffset int64  `json:"source_offset"`
	SHA256       string `json:"sha256"`
}

// Manifest is the per-block index of exported frames.
type Manifest struct {
	Cam        int        `json:"cam"`
	Index      int        `json:"index"`
	StartFrame int64      `json:"start_frame"`
	EndFrame   int64      `json:"end_frame"`
	Frames     []FrameRec `json:"frames"`
	SHA256     string     `json:"sha256"` // hash of concatenated frame hashes
}

func blockDir(root string, cam, index int) string {
	return filepath.Join("out", "frames", fmt.Sprintf("cam%02d", cam), fmt.Sprintf("block_%04d", index))
}

// frameBytes builds the deterministic offscreen pixel payload of one frame
// (a binary PGM whose pixels derive only from the frame's identity).
func frameBytes(seed string, cam int, frame, srcOff int64, tc string) []byte {
	header := fmt.Sprintf("P5\n16 16\n255\n# cam=%d frame=%d src=%d tc=%s\n", cam, frame, srcOff, tc)
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d|%s", seed, cam, frame, srcOff, tc)))
	out := []byte(header)
	for len(out) < len(header)+256 {
		out = append(out, h[:]...)
		h = sha256.Sum256(h[:])
	}
	return out[:len(header)+256]
}

// sourceOffset finds the source byte offset covering a frame: the first
// cue span containing it, else the nearest timecode record, else -1.
func sourceOffset(blk timeline.Block, frame int64) int64 {
	for _, sp := range blk.Spans {
		if frame >= sp.StartFrame && frame < sp.EndFrame {
			return sp.ByteOffset
		}
	}
	var best int64 = -1
	var bestDist int64 = 1 << 62
	for _, tc := range blk.Timecodes {
		if d := abs64(tc.AbsFrame - frame); d < bestDist {
			best, bestDist = tc.ByteOffset, d
		}
	}
	return best
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RenderBlock exports every frame of the block and its manifest.
func RenderBlock(root string, blk timeline.Block, r clock.Rate, drop bool, seed string) (*Manifest, error) {
	dir, err := safepath.Join(root, blockDir(root, blk.Cam, blk.Index))
	if err != nil {
		return nil, err
	}
	man := &Manifest{Cam: blk.Cam, Index: blk.Index, StartFrame: blk.StartFrame, EndFrame: blk.EndFrame}
	bh := sha256.New()
	for f := blk.StartFrame; f < blk.EndFrame; f++ {
		tc, err := clock.FormatTimecode(f, r, drop)
		if err != nil {
			return nil, err
		}
		src := sourceOffset(blk, f)
		payload := frameBytes(seed, blk.Cam, f, src, tc)
		sum := sha256.Sum256(payload)
		name := filepath.Join(dir, fmt.Sprintf("frame_%06d.pgm", f))
		if err := writeAtomic(name, payload); err != nil {
			return nil, err
		}
		hexSum := hex.EncodeToString(sum[:])
		bh.Write(sum[:])
		man.Frames = append(man.Frames, FrameRec{Frame: f, MediaTime: tc, SourceOffset: src, SHA256: hexSum})
	}
	man.SHA256 = hex.EncodeToString(bh.Sum(nil))
	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(dir, "manifest.json"), data); err != nil {
		return nil, err
	}
	return man, nil
}

// LoadManifest reads a block manifest from disk.
func LoadManifest(root string, cam, index int) (*Manifest, error) {
	p, err := safepath.Join(root, filepath.Join(blockDir(root, cam, index), "manifest.json"))
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// VerifyBlock recomputes every frame hash and the block hash, comparing
// against the manifest and the expected checkpoint checksum.
func VerifyBlock(root string, cam, index int, expectedSHA string) (bool, error) {
	man, err := LoadManifest(root, cam, index)
	if err != nil {
		return false, nil // unreadable manifest -> not safe
	}
	dir, err := safepath.Join(root, blockDir(root, cam, index))
	if err != nil {
		return false, err
	}
	bh := sha256.New()
	for _, fr := range man.Frames {
		payload, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("frame_%06d.pgm", fr.Frame)))
		if err != nil {
			return false, nil
		}
		sum := sha256.Sum256(payload)
		if hex.EncodeToString(sum[:]) != fr.SHA256 {
			return false, nil
		}
		bh.Write(sum[:])
	}
	if hex.EncodeToString(bh.Sum(nil)) != man.SHA256 {
		return false, nil
	}
	return man.SHA256 == expectedSHA, nil
}

// Aggregate folds all block manifests (sorted by cam, index) into the final
// review hash and the total exported frame count.
func Aggregate(mans []*Manifest) (string, int) {
	h := sha256.New()
	total := 0
	for _, m := range mans {
		sum, _ := hex.DecodeString(m.SHA256)
		h.Write(sum)
		total += len(m.Frames)
	}
	return hex.EncodeToString(h.Sum(nil)), total
}
