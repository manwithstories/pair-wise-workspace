// Package sample generates self-contained MP4 fixtures: normal, truncated,
// corrupted and boundary material. No external files are referenced; all
// bytes are derived from the caller's seed.
package sample

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"aircheck/internal/remux"
	"aircheck/internal/scan"
)

// Kind identifies a generated fixture.
type Kind string

const (
	KindNormal    Kind = "normal"
	KindTruncated Kind = "truncated"
	KindEdge      Kind = "edge"
)

// File describes one generated sample.
type File struct {
	Kind Kind
	Path string
}

// payloadStore is an in-memory ReaderAt of generated sample bytes.
type payloadStore struct{ data []byte }

func (p *payloadStore) ReadAt(b []byte, off int64) (int, error) {
	if off >= int64(len(p.data)) {
		return 0, fmt.Errorf("payload offset %d beyond %d", off, len(p.data))
	}
	n := copy(b, p.data[off:])
	return n, nil
}

// Generate writes the three demo fixtures into dir and returns them in
// deterministic order: normal, truncated, edge.
func Generate(seed int64, dir string) ([]File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var out []File

	normal, err := buildNormal(seed)
	if err != nil {
		return nil, err
	}
	if err := write(filepath.Join(dir, "normal.mp4"), normal); err != nil {
		return nil, err
	}
	out = append(out, File{Kind: KindNormal, Path: filepath.Join(dir, "normal.mp4")})

	// Truncated: keep the intact prefix of the normal file.
	cut := len(normal) * 3 / 5
	if err := write(filepath.Join(dir, "truncated.mp4"), normal[:cut]); err != nil {
		return nil, err
	}
	out = append(out, File{Kind: KindTruncated, Path: filepath.Join(dir, "truncated.mp4")})

	edge, err := buildEdge(seed)
	if err != nil {
		return nil, err
	}
	if err := write(filepath.Join(dir, "edge.mp4"), edge); err != nil {
		return nil, err
	}
	out = append(out, File{Kind: KindEdge, Path: filepath.Join(dir, "edge.mp4")})

	return out, nil
}

func write(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

// trackSpec describes a synthetic track to render.
type trackSpec struct {
	id        uint32
	mediaType string
	timescale uint32
	deltas    []uint32 // per-sample duration in track units
	keysEvery int      // keyframe interval in samples (video)
	startDTS  int64
	size      int // bytes per sample
}

// render produces the MP4 bytes for the given tracks.
func render(seed int64, specs []trackSpec) ([]byte, error) {
	rng := rand.New(rand.NewSource(seed))
	var tds []remux.TrackData
	var payload []byte
	base := int64(0) // payload offsets are relative to the store
	for _, sp := range specs {
		td := remux.TrackData{ID: sp.id, MediaType: sp.mediaType, Timescale: sp.timescale}
		dts := sp.startDTS
		for i, d := range sp.deltas {
			sz := sp.size + rng.Intn(sp.size/4+1)
			td.Packets = append(td.Packets, scan.Packet{
				TrackID:  sp.id,
				Index:    uint32(i + 1),
				Offset:   base,
				Size:     uint32(sz),
				DTS:      dts,
				PTS:      dts,
				Keyframe: sp.mediaType != "video" || (sp.keysEvery > 0 && i%sp.keysEvery == 0),
			})
			for j := 0; j < sz; j++ {
				payload = append(payload, byte(rng.Intn(256)))
			}
			base += int64(sz)
			dts += int64(d)
		}
		tds = append(tds, td)
	}
	var buf bytes.Buffer
	if err := remux.WriteContainer(&buf, &payloadStore{data: payload}, tds); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildNormal creates well-formed A/V material: 25 fps video at 12800
// timescale, AAC-like audio at 48000, aligned at t=0 (A/V error 0 ms).
func buildNormal(seed int64) ([]byte, error) {
	video := trackSpec{id: 1, mediaType: "video", timescale: 12800, keysEvery: 10, size: 2048}
	for i := 0; i < 50; i++ {
		video.deltas = append(video.deltas, 512) // 25 fps
	}
	audio := trackSpec{id: 2, mediaType: "audio", timescale: 48000, size: 256}
	for i := 0; i < 94; i++ {
		audio.deltas = append(audio.deltas, 1024) // ~2 s of audio
	}
	return render(seed, []trackSpec{video, audio})
}

// buildEdge creates boundary material: variable frame rate, a keyframe gap
// above the common threshold, and a DTS origin near the 32-bit wrap point.
func buildEdge(seed int64) ([]byte, error) {
	video := trackSpec{
		id: 1, mediaType: "video", timescale: 90000, keysEvery: 35, size: 1500,
		startDTS: 0xFFFFFF00, // near uint32 wrap to exercise unwrapping
	}
	for i := 0; i < 40; i++ {
		d := uint32(3000) // 30 fps
		if i%7 == 3 {
			d = 1500 // VFR spike
		}
		video.deltas = append(video.deltas, d)
	}
	// keysEvery 35 over 40 samples: keyframes at 0 and 35, leaving a
	// 34-sample gap above common thresholds.
	return render(seed, []trackSpec{video})
}
