// Package testdata deterministically generates archive fixtures: healthy
// HLS/DASH assets (with migration artifacts like duplicate, conflicting
// and out-of-order segments), corrupt cases (truncated box, empty
// segment, missing segment, clock overflow) and RTP transport streams
// with burst loss, reordering and duplicates. No external files are used.
package testdata

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
)

// Config controls fixture generation.
type Config struct {
	Seed      int64
	BurstLoss int       // consecutive RTP packets dropped in one burst
	Reorder   int       // RTP reorder window
	Bandwidth []float64 // bandwidth trace in Mbps
}

// Asset is one generated asset directory.
type Asset struct {
	Name         string
	Kind         string // "hls" or "dash"
	Dir          string
	ManifestFile string // file name of the entry manifest inside Dir
}

// CorruptCase is an asset expected to fail with a locatable error.
type CorruptCase struct {
	Name         string
	Kind         string
	Dir          string
	ManifestFile string
	Want         string // expected error kind: "demux", "segment", "clock", "io"
}

// Fixtures is everything the demo pipeline needs.
type Fixtures struct {
	Root    string
	Good    []Asset
	Corrupt []CorruptCase
	RTP     [][]byte // transmitted RTP datagrams (after loss/reorder/dup)
	RTPBase uint16   // first sequence number
}

// Generate builds all fixtures under root.
func Generate(cfg Config, root string) (*Fixtures, error) {
	rng := rand.New(rand.NewSource(cfg.Seed))
	f := &Fixtures{Root: root}
	type gen struct {
		name string
		kind string
		fn   func(dir string) (manifest string, err error)
	}
	goods := []gen{
		{"episode-hls", "hls", func(dir string) (string, error) { return genHLSAsset(rng, dir, false) }},
		{"docu-dash", "dash", func(dir string) (string, error) { return genDASHAsset(rng, dir) }},
		{"wrap-hls", "hls", func(dir string) (string, error) { return genHLSAsset(rng, dir, true) }},
		{"big-dash", "dash", func(dir string) (string, error) { return genBigAsset(rng, dir) }},
		{"classic-mp4", "dash", func(dir string) (string, error) { return genClassicMP4(rng, dir) }},
	}
	for _, g := range goods {
		dir := filepath.Join(root, g.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		mf, err := g.fn(dir)
		if err != nil {
			return nil, fmt.Errorf("generate %s: %w", g.name, err)
		}
		f.Good = append(f.Good, Asset{Name: g.name, Kind: g.kind, Dir: dir, ManifestFile: mf})
	}
	corrupts := []gen{
		{"corrupt-truncated", "hls", func(dir string) (string, error) { return genTruncated(rng, dir) }},
		{"corrupt-empty", "hls", func(dir string) (string, error) { return genEmpty(rng, dir) }},
		{"corrupt-missing-seg", "hls", func(dir string) (string, error) { return genMissingSeg(rng, dir) }},
		{"corrupt-clock", "hls", func(dir string) (string, error) { return genClockOverflow(rng, dir) }},
	}
	want := []string{"demux", "demux", "segment", "clock"}
	for i, g := range corrupts {
		dir := filepath.Join(root, g.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		mf, err := g.fn(dir)
		if err != nil {
			return nil, fmt.Errorf("generate %s: %w", g.name, err)
		}
		f.Corrupt = append(f.Corrupt, CorruptCase{Name: g.name, Kind: g.kind, Dir: dir, ManifestFile: mf, Want: want[i]})
	}
	// RTP transport stream with burst loss, reordering and duplicates
	f.RTPBase = 65520 // forces a 16-bit sequence wrap mid-stream
	f.RTP = RTPStream(rng, f.RTPBase, 300, cfg.BurstLoss, cfg.Reorder)
	return f, nil
}

func writeFile(dir, name string, data []byte) error {
	return os.WriteFile(filepath.Join(dir, name), data, 0o644)
}
