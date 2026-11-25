// Package layout defines the on-disk directory and naming convention
// shared by the fixture generator, pipeline, index and independent
// auditor. Keeping it in one place prevents path drift.
package layout

import (
	"fmt"
	"path/filepath"
)

const (
	InputDir   = "in"
	OutputDir  = "out"
	HLSName    = "index.m3u8"
	DASHName   = "manifest.mpd"
	IndexName  = "index.json"
	ReportName = "report.json"
	FECName    = "rtp/fec.json"
	CaptureName = "rtp/capture.rtpbin"
	TracksMetaName = "meta/tracks.json"
	StructuralName = "structural/truncated_trun.m4s"
)

// Tracks in deterministic processing order.
var Tracks = []string{"video", "audio"}

// TimescaleFor gives the fixture timescale for a track.
func TimescaleFor(track string) uint32 {
	if track == "audio" {
		return 48000
	}
	return 90000
}

// SamplesPerSegment gives the constant sample count used by fixtures.
func SamplesPerSegment(track string) int {
	if track == "audio" {
		return 48 // 48 * 1000 ticks @48k = 1s
	}
	return 30 // 30 * 3000 ticks @90k = 1s
}

// DurationTick gives per-sample duration in track timescale.
func DurationTick(track string) uint32 {
	if track == "audio" {
		return 1000
	}
	return 3000
}

func GroupDir(root, group string) string { return filepath.Join(root, group) }
func InDir(root, group string) string    { return filepath.Join(GroupDir(root, group), InputDir) }
func OutDir(root, group string) string   { return filepath.Join(GroupDir(root, group), OutputDir) }

func HLSPath(root, group string) string  { return filepath.Join(InDir(root, group), HLSName) }
func DASHPath(root, group string) string { return filepath.Join(InDir(root, group), DASHName) }
func IndexPath(root, group string) string {
	return filepath.Join(OutDir(root, group), IndexName)
}
func ReportPath(root, group string) string {
	return filepath.Join(GroupDir(root, group), ReportName)
}

// SegmentName is the zero-padded segment file name.
func SegmentName(index int) string { return fmt.Sprintf("seg_%03d.m4s", index) }

// SourceRel is the input-relative segment path stored in the index.
func SourceRel(track string, index int) string {
	return filepath.ToSlash(filepath.Join(track, SegmentName(index)))
}

// OutputRel mirrors SourceRel under the output directory.
func OutputRel(track string, index int) string { return SourceRel(track, index) }

func SourcePath(root, group, track string, index int) string {
	return filepath.Join(InDir(root, group), filepath.FromSlash(SourceRel(track, index)))
}

func OutputPath(root, group, track string, index int) string {
	return filepath.Join(OutDir(root, group), filepath.FromSlash(OutputRel(track, index)))
}

func CapturePath(root, group string) string {
	return filepath.Join(InDir(root, group), filepath.FromSlash(CaptureName))
}
func FECPath(root, group string) string {
	return filepath.Join(InDir(root, group), filepath.FromSlash(FECName))
}
func TracksMetaPath(root, group string) string {
	return filepath.Join(InDir(root, group), filepath.FromSlash(TracksMetaName))
}
func StructuralPath(root, group string) string {
	return filepath.Join(InDir(root, group), filepath.FromSlash(StructuralName))
}
