// Package fixture generates every archive test container locally. No external
// media is referenced: payloads are deterministic pseudo-random byte streams
// sized against a target bit rate, and all clock anomalies (drift, 32-bit
// wraparound, VFR) and damage cases are synthesized from explicit rules.
package fixture

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"

	"mediaarchive/internal/demux"
	"mediaarchive/internal/model"
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

// GenerateAll writes every requested case into dir. "all" expands AllCases.
func GenerateAll(dir string, cases []string, seed int64) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.NewError(model.StageCLI, "cannot create fixture dir").Wrap(err)
	}
	if len(cases) == 1 && cases[0] == "all" {
		cases = AllCases
	}
	var paths []string
	for _, name := range cases {
		spec, err := specFor(name, seed)
		if err != nil {
			return nil, err
		}
		p := filepath.Join(dir, name+".mov")
		if err := buildAndWrite(spec, p); err != nil {
			return nil, err
		}
		if err := writeMappingSidecar(spec, filepath.Join(dir, name+".map.json")); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// writeMappingSidecar emits explicit mapping rules. The conflict case gets a
// duplicated track id so track.Build must reject it with a locatable error.
func writeMappingSidecar(spec Spec, path string) error {
	type rule struct {
		TrackID uint32 `json:"track_id"`
		Role    string `json:"role"`
	}
	var rules []rule
	for _, t := range spec.Tracks {
		if t.Kind == demux.KindHint {
			continue
		}
		role := "audio"
		if t.Kind == demux.KindVideo {
			role = "video"
		}
		rules = append(rules, rule{TrackID: t.ID, Role: role})
	}
	if spec.BadMapping && len(rules) > 0 {
		// Duplicate the first rule: same track id mapped twice.
		rules = append(rules, rules[0])
	}
	b, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return model.NewError(model.StageCLI, "mapping sidecar encode failed").Wrap(err)
	}
	return os.WriteFile(path, b, 0o644)
}

func specFor(name string, seed int64) (Spec, error) {
	switch name {
	case CaseNormal:
		return Spec{
			Name: name, Seed: seed, QuickTime: true, TargetBPS: 4_000_000,
			Tracks: []TrackSpec{
				{ID: 1, Kind: demux.KindVideo, Timescale: 90000, StartTime: 1500,
					Samples: 48, Codec: "synthetic-video"},
				{ID: 2, Kind: demux.KindAudio, Timescale: 48000, StartTime: 240,
					Samples: 48, Codec: "synthetic-audio"},
			},
		}, nil
	case CaseAnomaly:
		return Spec{
			Name: name, Seed: seed + 1, QuickTime: false, TargetBPS: 2_000_000,
			Tracks: []TrackSpec{
				{ID: 11, Kind: demux.KindVideo, Timescale: 90000, StartTime: 0,
					DriftPPM: 200, WrapUnits: 1 << 16, Samples: 200, VFR: true,
					Codec: "synthetic-video"},
				{ID: 12, Kind: demux.KindAudio, Timescale: 48000, StartTime: 0,
					DriftPPM: -150, Samples: 200, Codec: "synthetic-audio"},
			},
		}, nil
	case CaseTruncated:
		return Spec{
			Name: name, Seed: seed + 2, QuickTime: true, TargetBPS: 1_500_000,
			TruncateTail: 64,
			Tracks: []TrackSpec{
				{ID: 21, Kind: demux.KindVideo, Timescale: 90000, StartTime: 0,
					Samples: 30, Codec: "synthetic-video"},
			},
		}, nil
	case CaseConflict:
		return Spec{
			Name: name, Seed: seed + 3, QuickTime: false, TargetBPS: 1_000_000,
			BadMapping: true,
			Tracks: []TrackSpec{
				{ID: 31, Kind: demux.KindVideo, Timescale: 90000, StartTime: 0,
					Samples: 16, Codec: "synthetic-video"},
				{ID: 32, Kind: demux.KindAudio, Timescale: 48000, StartTime: 0,
					Samples: 16, Codec: "synthetic-audio"},
			},
		}, nil
	case CaseRTPGap:
		return Spec{
			Name: name, Seed: seed + 4, QuickTime: true, TargetBPS: 1_000_000,
			Tracks: []TrackSpec{
				{ID: 41, Kind: demux.KindHint, Timescale: 90000, StartTime: 0,
					Samples: 24, RTPGap: true, Codec: "rtp-hint"},
				{ID: 42, Kind: demux.KindVideo, Timescale: 90000, StartTime: 0,
					Samples: 24, Codec: "synthetic-video"},
			},
		}, nil
	default:
		return Spec{}, model.NewError(model.StageCLI, "unknown fixture case: "+name)
	}
}
