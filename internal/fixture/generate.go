package fixture

import (
	"fmt"
	"os"
	"path/filepath"
)

// Default media geometry shared by every generated representation.
const (
	DefaultWidth      = 320
	DefaultHeight     = 180
	DefaultFrameRate  = 25
	DefaultSampleRate = 48000
	DefaultChannels   = 2
)

// Options configures a generated dataset.
type Options struct {
	WorkDir     string
	Format      string // "hls", "dash", "both"
	Cameras     int
	DurationSec float64
	Profile     Profile
	// SegmentSec is the target segment duration (defaults to 2).
	SegmentSec float64
}

// Dataset describes a generated on-disk dataset.
type Dataset struct {
	Root       string
	Plan       *Plan
	Cameras    []CameraLayout
	SegmentSec float64
}

// CameraLayout records the on-disk playlist paths for one camera.
type CameraLayout struct {
	Camera    string
	Root      string
	HLSMaster string // absolute path, empty when HLS disabled
	DASHMPD   string // absolute path, empty when DASH disabled
}

// segmentCount derives the number of segments from duration / segment length.
func segmentCount(durationSec, segmentSec float64) int {
	if segmentSec <= 0 {
		segmentSec = 2
	}
	n := int(durationSec/segmentSec + 0.5)
	if n < 1 {
		n = 1
	}
	return n
}

// Generate builds the complete dataset (segments + playlists + faults).
func Generate(opt Options) (*Dataset, error) {
	if err := WarmUp(); err != nil {
		return nil, err
	}
	segmentSec := opt.SegmentSec
	if segmentSec <= 0 {
		segmentSec = 2
	}
	cameras := CameraNames(opt.Cameras)
	count := segmentCount(opt.DurationSec, segmentSec)
	plan := BuildPlan(opt.Profile, cameras, count)

	root := filepath.Join(opt.WorkDir, "fixture")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("fixture: mkdir: %w", err)
	}

	ds := &Dataset{Root: root, Plan: plan, SegmentSec: segmentSec}
	for _, cam := range cameras {
		layout, err := generateCamera(root, cam, opt, plan, count, segmentSec)
		if err != nil {
			return nil, err
		}
		ds.Cameras = append(ds.Cameras, layout)
	}
	return ds, nil
}

// generateCamera creates both representations' segments and playlists for one
// camera. Segment bytes are encoded once per (camera, sequence) and shared
// between HLS and DASH where geometry is identical.
func generateCamera(root, cam string, opt Options, plan *Plan, count int, segmentSec float64) (CameraLayout, error) {
	camRoot := filepath.Join(root, cam)
	if err := os.MkdirAll(camRoot, 0o755); err != nil {
		return CameraLayout{}, err
	}
	layout := CameraLayout{Camera: cam, Root: camRoot}

	// Encode and write primary and backup segments.
	for _, role := range []string{"primary", "backup"} {
		if err := writeRepresentationSegments(camRoot, cam, role, opt, plan, count, segmentSec); err != nil {
			return layout, err
		}
	}

	switch opt.Format {
	case "hls", "both":
		p, err := writeHLSPlaylists(camRoot, cam, plan, count, segmentSec)
		if err != nil {
			return layout, err
		}
		layout.HLSMaster = p
	}
	switch opt.Format {
	case "dash", "both":
		p, err := writeDASHMPD(camRoot, cam, plan, count, segmentSec)
		if err != nil {
			return layout, err
		}
		layout.DASHMPD = p
	}
	return layout, nil
}
