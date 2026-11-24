// Package remux repackages only fully valid fMP4 ranges. Damaged spans are
// never joined into a continuous timeline: each surviving fragment keeps its
// original manifest time range and the emitted manifests reference only those
// fragments.
package remux

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mediac/internal/core"
	"mediac/internal/inspect"
	"mediac/internal/manifest"
)

// Output is the set of artifacts produced for one representation.
type Output struct {
	RepID         string   `json:"rep_id"`
	InitPath      string   `json:"init_path"`
	FragmentPaths []string `json:"fragment_paths"`
	// SkippedSegments are damaged indexes excluded from the output timeline.
	SkippedSegments []int      `json:"skipped_segments"`
	ManifestHLS     string     `json:"manifest_hls"`
	ManifestDASH    string     `json:"manifest_dash"`
	Hashes          FileHashes `json:"hashes"`
}

// FileHashes pins output content so reruns can prove determinism.
type FileHashes struct {
	Init      string   `json:"init"`
	Fragments []string `json:"fragments"`
	HLS       string   `json:"hls_manifest"`
	DASH      string   `json:"dash_manifest"`
}

// Plan decides which fragments are eligible: a fragment is valid only when
// its inspection report is structurally clean and transport recovery marked
// it complete.
type Plan struct {
	Root      string
	OutDir    string
	Manifest  *manifest.Manifest
	RepIndex  int
	Reports   map[int]*inspect.FragmentReport // fragment index -> report
	Complete  map[int]bool                    // fragment index -> recovered fully
	InitBytes []byte
}

// keptSeg pairs a surviving manifest segment with its clean report.
type keptSeg struct {
	seg manifest.Segment
	rep *inspect.FragmentReport
}

// Run writes the remuxed representation. It never invents bytes for a gap and
// never renumbers surviving fragments into a gapless sequence.
func Run(p Plan) (*Output, error) {
	rep := p.Manifest.Representations[p.RepIndex]
	out := &Output{RepID: rep.ID}
	repDir := filepath.Join(p.OutDir, safeName(rep.ID))
	if err := os.MkdirAll(repDir, 0o755); err != nil {
		return nil, err
	}

	// Initialization segment preserved verbatim.
	initPath := filepath.Join(repDir, "init.mp4")
	if err := os.WriteFile(initPath, p.InitBytes, 0o644); err != nil {
		return nil, err
	}
	out.InitPath = initPath
	out.Hashes.Init = hashBytes(p.InitBytes)

	var keptSegs []keptSeg
	for i := range rep.Segments {
		seg := rep.Segments[i]
		fragIdx := segmentToFragment(seg)
		rpt := p.Reports[fragIdx]
		if !eligible(fragIdx, rpt, p.Complete) {
			out.SkippedSegments = append(out.SkippedSegments, seg.Index)
			continue
		}
		data, err := os.ReadFile(seg.AbsPath)
		if err != nil {
			return nil, core.NewFault(core.FaultBlocked, core.SevBlocking,
				"cannot read valid fragment: "+err.Error(),
				core.Source{RepID: rep.ID, SegmentIndex: seg.Index})
		}
		name := fmt.Sprintf("seg-%d.m4s", seg.Index)
		full := filepath.Join(repDir, name)
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return nil, err
		}
		out.FragmentPaths = append(out.FragmentPaths, full)
		out.Hashes.Fragments = append(out.Hashes.Fragments, hashBytes(data))
		keptSegs = append(keptSegs, keptSeg{seg: seg, rep: rpt})
	}
	sort.Ints(out.SkippedSegments)

	hls := renderHLS(rep, keptSegs)
	dash := renderDASH(rep, keptSegs)
	hlsPath := filepath.Join(repDir, "index.m3u8")
	dashPath := filepath.Join(repDir, "stream.mpd")
	if err := os.WriteFile(hlsPath, hls, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(dashPath, dash, 0o644); err != nil {
		return nil, err
	}
	out.ManifestHLS, out.ManifestDASH = hlsPath, dashPath
	out.Hashes.HLS = hashBytes(hls)
	out.Hashes.DASH = hashBytes(dash)
	return out, nil
}

func eligible(fragIdx int, rpt *inspect.FragmentReport, complete map[int]bool) bool {
	if rpt == nil {
		return false
	}
	if rpt.Truncated || rpt.HasInvalidNAL || rpt.HasZeroDur {
		return false
	}
	if complete != nil && !complete[fragIdx] {
		return false
	}
	return true
}

// segmentToFragment maps a manifest segment sequence to the fragment ordinal
// used by transport/inspection. In this tool they share the same numbering.
func segmentToFragment(seg manifest.Segment) int { return seg.Index }

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func safeName(id string) string {
	replacer := func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ', '.':
			return '_'
		}
		return r
	}
	return filepath.Clean(string(mapBytes(id, replacer)))
}

func mapBytes(s string, f func(rune) rune) []byte {
	var b bytes.Buffer
	for _, r := range s {
		b.WriteRune(f(r))
	}
	return b.Bytes()
}
