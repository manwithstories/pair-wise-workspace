package fixture

import (
	"math/rand"

	"mediaarchive/internal/demux"
)

// Case names accepted by the CLI.
const (
	CaseNormal    = "normal"
	CaseAnomaly   = "anomaly"
	CaseTruncated = "truncated"
	CaseConflict  = "conflict"
	CaseRTPGap    = "rtpgap"
)

// AllCases lists every generator in demo order.
var AllCases = []string{CaseNormal, CaseAnomaly, CaseTruncated, CaseConflict, CaseRTPGap}

// Spec describes one synthesized container.
type Spec struct {
	Name         string
	Seed         int64
	QuickTime    bool
	TargetBPS    int64
	Tracks       []TrackSpec
	TruncateTail int64 // bytes removed from EOF (truncated case)
	// BadMapping is written into a sidecar for the conflict case.
	BadMapping bool
}

// TrackSpec is one synthesized track.
type TrackSpec struct {
	ID        uint32
	Kind      string
	Timescale uint32
	StartTime uint64
	DriftPPM  int32
	WrapUnits uint32
	Samples   int
	// VFR makes durations alternate between two values.
	VFR bool
	// RTPGap inserts a missing RTP sequence number mid-track.
	RTPGap bool
	Codec  string
}

type sampleRec struct {
	demux.Sample
	payload []byte
}

type builder struct {
	spec   Spec
	rng    *rand.Rand
	tracks []demux.TrackMeta
	// tables per chunk: each inner slice keeps sample records in order.
	tables    [][]sampleRec
	offsetPos []int
	lastArmd  []byte
}
