// Package render produces the offscreen frame sequence: each video
// packet is decoded, normalized (crop then display rotation) and written
// as a JPEG with a per-frame SHA-256.
package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"

	"archive/internal/media"
	"archive/internal/normalize"
	"archive/internal/worker"
)

// Frame is one rendered output frame with its provenance.
type Frame struct {
	Index     int
	Name      string
	SHA256    string
	PTS       int64
	Track     int
	Seq       int64
	LatencyMs int64
	Rules     string
}

// Frames renders every non-lost video packet in pkts (timeline order)
// into outDir. Work is distributed over the pool but results are
// re-sorted by frame index, so output is independent of worker count.
// On cancellation or failure every file created in outDir is removed.
func Frames(ctx context.Context, pool *worker.Pool, pkts []*media.Packet, dec *normalize.Decision, outDir string) ([]Frame, error) {
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return nil, err
	}
	rules := "none"
	if len(dec.Rules) > 0 {
		rules = strings.Join(dec.Rules, ";")
	}
	results := make([]Frame, len(pkts))
	var mu sync.Mutex
	var created []string
	cleanup := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, f := range created {
			_ = os.Remove(f)
		}
	}
	tasks := make([]func(context.Context) error, len(pkts))
	for i, p := range pkts {
		i, p := i, p
		tasks[i] = func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			start := time.Now()
			fr, path, err := renderOne(i, p, dec, rules, outDir)
			if err != nil {
				return err
			}
			fr.LatencyMs = time.Since(start).Milliseconds()
			mu.Lock()
			created = append(created, path)
			results[i] = fr
			mu.Unlock()
			return nil
		}
	}
	if err := pool.Run(ctx, tasks); err != nil {
		cleanup()
		return nil, err
	}
	out := make([]Frame, 0, len(results))
	for _, fr := range results {
		if fr.Name != "" {
			out = append(out, fr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	return out, nil
}

func renderOne(idx int, p *media.Packet, dec *normalize.Decision, rules, outDir string) (Frame, string, error) {
	var fr Frame
	img, err := jpeg.Decode(bytes.NewReader(p.Data))
	if err != nil {
		return fr, "", media.Faultf(media.StageRender, p.Track, p.Offset,
			"JPEG decode failed for seq %d: %v", p.Seq, err)
	}
	// clap crop is expressed in coded-frame coordinates: crop first,
	// then apply the display rotation.
	if dec.Crop != nil {
		r := image.Rect(dec.Crop.X, dec.Crop.Y, dec.Crop.X+dec.Crop.W, dec.Crop.Y+dec.Crop.H)
		if !r.In(img.Bounds()) {
			return fr, "", media.Faultf(media.StageRender, p.Track, p.Offset,
				"crop %v outside image bounds %v", r, img.Bounds())
		}
		img = imaging.Crop(img, r)
	}
	switch dec.Rotation {
	case 90:
		img = imaging.Rotate90(img)
	case 180:
		img = imaging.Rotate180(img)
	case 270:
		img = imaging.Rotate270(img)
	}
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.JPEG, imaging.JPEGQuality(90)); err != nil {
		return fr, "", media.Faultf(media.StageRender, p.Track, p.Offset, "JPEG encode: %v", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	name := fmt.Sprintf("frame_%04d.jpg", idx)
	path := filepath.Join(outDir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fr, "", err
	}
	return Frame{
		Index: idx, Name: name, SHA256: hex.EncodeToString(sum[:]),
		PTS: p.PTS, Track: p.Track, Seq: p.Seq, Rules: rules,
	}, path, nil
}
