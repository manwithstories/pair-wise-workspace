package fixture

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gomp4 "github.com/abema/go-mp4"

	"mediaroute/internal/model"
)

// Spec describes a synthetic capture session to generate.
type Spec struct {
	Seed              int64
	Minutes           int
	Cameras           int
	Bitrates          []uint32 // kbps tier labels
	Dir               string
	SampleIntervalSec int // default 4
	SegmentSec        int // default 60 (== keyframe interval)
}

// BoundaryFault locates the injected cross-bitrate cut-point offset.
type BoundaryFault struct {
	Camera  string
	Bitrate uint32
	Segment int
}

// Generated describes what was generated and which faults to expect.
type Generated struct {
	ManifestPath    string
	BaseDir         string
	ContainerFaults int // recoverable container faults injected
	Boundary        BoundaryFault
	SegmentSec      int
}

// tierTimescales spreads representations over distinct media timescales
// to exercise rational clock conversion.
var tierTimescales = []uint32{90000, 48000, 30000}

// Generate creates all sample files and the manifest under Spec.Dir.
func Generate(spec Spec) (*Generated, error) {
	if spec.SampleIntervalSec <= 0 {
		spec.SampleIntervalSec = 4
	}
	if spec.SegmentSec <= 0 {
		spec.SegmentSec = 60
	}
	if len(spec.Bitrates) == 0 {
		spec.Bitrates = []uint32{100, 200, 400}
	}
	if err := os.MkdirAll(filepath.Join(spec.Dir, "media"), 0o755); err != nil {
		return nil, model.New(model.CodeIO, "fixture mkdir", err)
	}
	n := spec.Minutes * 60 / spec.SampleIntervalSec
	kfEvery := spec.SegmentSec / spec.SampleIntervalSec
	boundaryTier := 2
	if len(spec.Bitrates) < 3 {
		boundaryTier = len(spec.Bitrates) - 1
	}
	g := &Generated{BaseDir: spec.Dir, SegmentSec: spec.SegmentSec}
	g.Boundary.Segment = 4 + int(spec.Seed%4)
	if maxSeg := n/kfEvery - 2; g.Boundary.Segment > maxSeg {
		g.Boundary.Segment = maxSeg // keep the anomaly inside the media
	}
	if g.Boundary.Segment < 1 {
		g.Boundary.Segment = 1
	}
	var lines []string
	for cam := 0; cam < spec.Cameras; cam++ {
		for tier, br := range spec.Bitrates {
			ts := tierTimescales[tier%len(tierTimescales)]
			t := spec.trackFor(cam, tier, ts, n, kfEvery)
			camera := fmt.Sprintf("cam%d", cam)
			g.applyFaults(&t, camera, cam, tier, boundaryTier, br, ts, n, kfEvery)
			name := fmt.Sprintf("media/%s_%d.mp4", camera, br)
			if err := writeTrack(filepath.Join(spec.Dir, name), t); err != nil {
				return nil, err
			}
			lines = append(lines, fmt.Sprintf(
				"camera=%s bitrate=%d timescale=%d path=%s", camera, br, ts, name))
		}
	}
	g.ManifestPath = filepath.Join(spec.Dir, "manifest.txt")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(g.ManifestPath, []byte(content), 0o644); err != nil {
		return nil, model.New(model.CodeIO, "fixture manifest", err)
	}
	return g, nil
}

func (spec Spec) trackFor(cam, tier int, ts uint32, n, kfEvery int) trackSpec {
	sizes := make([]uint32, n)
	for i := range sizes {
		sizes[i] = sampleSize(spec.Seed, cam, tier, i)
	}
	return trackSpec{
		timescale:     ts,
		delta:         uint32(spec.SampleIntervalSec) * ts,
		sizes:         sizes,
		keyframeEvery: kfEvery,
		payloadAt: func(idx int) []byte {
			return samplePayload(spec.Seed, cam, tier, idx, int(sizes[idx]))
		},
	}
}

// applyFaults injects the deterministic fault set. With >=4 cameras and
// >=3 tiers exactly five recoverable container faults plus one
// cross-bitrate boundary offset are injected.
func (g *Generated) applyFaults(t *trackSpec, camera string, cam, tier, boundaryTier int,
	br, ts uint32, n, kfEvery int) {
	delta := t.delta
	dur := uint64(n) * uint64(delta)
	switch {
	case cam == 1 && tier == 0:
		t.overflowTrailer = true
		g.ContainerFaults++
	case cam == 1 && tier == 1:
		t.declareExtra = 50000
		for _, s := range t.sizes[len(t.sizes)-5:] { // last samples missing
			t.truncateTail += uint64(s)
		}
		t.truncateTail += 7 // cut mid-sample
		g.ContainerFaults++
	case cam == 2 && tier == 0:
		base := 2 * n / 3 // zero-delta run, compensated inside one segment
		t.stts = []gomp4.SttsEntry{
			{SampleCount: uint32(base), SampleDelta: delta},
			{SampleCount: 3, SampleDelta: 0},
			{SampleCount: 3, SampleDelta: 2 * delta},
			{SampleCount: uint32(n - base - 6), SampleDelta: delta},
		}
		g.ContainerFaults++
	case cam == 2 && tier == 1:
		t.elst = makeElst(dur, int64(dur)+1000*int64(ts)) // out of range
		g.ContainerFaults++
	case cam == 3 && tier == 0:
		t.emptyStss = true
		g.ContainerFaults++
	case cam == 3 && tier == 1: // valid non-zero start, rebased by repair
		t.elst = makeElst(dur, int64(2*delta))
		for i := 2; i < n; i += kfEvery {
			t.sync = append(t.sync, uint32(i+1))
		}
	case cam == 0 && tier == boundaryTier:
		k := g.Boundary.Segment * kfEvery
		t.stts = []gomp4.SttsEntry{ // one boundary sample lands 6 s late
			{SampleCount: uint32(k - 1), SampleDelta: delta},
			{SampleCount: 1, SampleDelta: 10 * ts},
			{SampleCount: 2, SampleDelta: 1 * ts},
			{SampleCount: uint32(n - k - 2), SampleDelta: delta},
		}
		g.Boundary.Camera = camera
		g.Boundary.Bitrate = br
	}
}

func makeElst(dur uint64, mediaTime int64) *gomp4.Elst {
	return &gomp4.Elst{
		EntryCount: 1,
		Entries: []gomp4.ElstEntry{{
			SegmentDurationV0: uint32(dur),
			MediaTimeV0:       int32(mediaTime),
			MediaRateInteger:  1,
		}},
	}
}

// sampleSize derives a deterministic per-sample size in bytes.
func sampleSize(seed int64, cam, tier, idx int) uint32 {
	h := hashFields(seed, cam, tier, idx, 0)
	return uint32(400 + 250*tier + int(h[0]))
}

// samplePayload derives deterministic sample content.
func samplePayload(seed int64, cam, tier, idx, size int) []byte {
	out := make([]byte, 0, size+32)
	var ctr uint64
	for len(out) < size {
		sum := hashFields(seed, cam, tier, idx, ctr)
		out = append(out, sum[:]...)
		ctr++
	}
	return out[:size]
}

func hashFields(seed int64, cam, tier, idx int, ctr uint64) [32]byte {
	var b [40]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(seed))
	binary.BigEndian.PutUint64(b[8:16], uint64(cam))
	binary.BigEndian.PutUint64(b[16:24], uint64(tier))
	binary.BigEndian.PutUint64(b[24:32], uint64(idx))
	binary.BigEndian.PutUint64(b[32:40], ctr)
	return sha256.Sum256(b[:])
}
