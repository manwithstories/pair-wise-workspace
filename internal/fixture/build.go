// Package fixture generates all delivery material on demand: DASH/HLS
// manifests, fMP4 init and media fragments, and RTP capture records. Nothing
// references external files, and every generated sample is deterministic so
// reruns reproduce identical summaries and hashes.
package fixture

import (
	"os"
	"path/filepath"
)

// Layout names the three target bitrates and codec parameters shared by the
// clean sample and every fault injection.
var Layout = []Spec{
	{ID: "v-400", Codec: "avc1.640028", Width: 640, Height: 360, BPS: 400000},
	{ID: "v-800", Codec: "avc1.640028", Width: 1280, Height: 720, BPS: 800000},
	{ID: "v-1600", Codec: "hvc1.1.6.L120", Width: 1920, Height: 1080, BPS: 1600000},
}

// Spec describes one representation of the generated program.
type Spec struct {
	ID     string
	Codec  string
	Width  int
	Height int
	BPS    int64
}

// Case names match the acceptance demo selector.
const (
	CaseClean        = "clean"
	CaseDuplicate    = "duplicate"
	CaseReorder      = "reorder"
	CaseBurstLoss    = "burst-loss"
	CaseTruncatedBox = "truncated-box"
	CaseClockReset   = "clock-reset"
)

// AllCases is the canonical demo order.
var AllCases = []string{
	CaseClean, CaseDuplicate, CaseReorder, CaseBurstLoss, CaseTruncatedBox, CaseClockReset,
}

// SegmentCount is the per-representation fragment count in every sample.
const SegmentCount = 24

// SamplesPerFragment and the timescale give a stable 1-second GOP cadence.
const (
	SamplesPerFragment = 4
	Timescale          = 1000
	SampleDuration     = 250 // ticks; 4 samples = 1000 ticks = 1 s
)

// Build materializes one complete case under root/<case>. It returns the
// generated tree descriptor used by the demo orchestrator.
func Build(root, caseName string) (*Tree, error) {
	dir := filepath.Join(root, caseName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tree := &Tree{Root: dir, Case: caseName}
	switch caseName {
	case CaseClean:
		if err := buildClean(tree); err != nil {
			return nil, err
		}
	case CaseDuplicate, CaseReorder, CaseBurstLoss, CaseTruncatedBox, CaseClockReset:
		if err := buildFault(tree, caseName); err != nil {
			return nil, err
		}
	default:
		return nil, errUnknownCase(caseName)
	}
	return tree, nil
}

// Tree describes the generated files for one case.
type Tree struct {
	Root        string
	Case        string
	DASHMPD     string
	HLSMaster   string
	HLSMedia    map[string]string // rep id -> media playlist path
	InitFiles   map[string]string
	FragmentDir map[string]string
	RTPRecord   string
}

func (t *Tree) mkdir(parts ...string) (string, error) {
	p := filepath.Join(append([]string{t.Root}, parts...)...)
	return p, os.MkdirAll(p, 0o755)
}
