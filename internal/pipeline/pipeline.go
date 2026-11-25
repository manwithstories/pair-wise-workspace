// Package pipeline is the batch adapter orchestrating generation, parsing,
// timeline replay, remuxing and audit manifest creation.
package pipeline

import (
	"fmt"
	"os"
	"path/filepath"

	"mediqc/internal/audit"
	"mediqc/internal/checkpoint"
	"mediqc/internal/fixture"
	"mediqc/internal/media"
)

// Options configures one batch run.
type Options struct {
	Seed  int64
	Suite string
	Out   string
}

// Run executes the requested deterministic suite and writes its manifest.
func Run(opts Options) (*audit.SuiteManifest, *media.Error) {
	if opts.Suite != "all" {
		return nil, &media.Error{
			Code: media.CodeUnsupported, Component: "pipeline",
			Message: fmt.Sprintf("suite %q is outside supported range", opts.Suite),
		}
	}
	if opts.Out == "" {
		return nil, &media.Error{Code: media.CodeInternal, Component: "pipeline", Message: "output directory required"}
	}
	if err := os.MkdirAll(filepath.Join(opts.Out, "cases"), 0o755); err != nil {
		return nil, &media.Error{Code: media.CodeInternal, Component: "pipeline", Message: err.Error()}
	}
	cases, merr := fixture.Suite(opts.Seed)
	if merr != nil {
		return nil, merr
	}

	suite := audit.SuiteManifest{
		SchemaVersion:   1,
		Seed:            opts.Seed,
		Cases:           len(cases),
		ReplayHashEqual: true,
	}
	for _, c := range cases {
		cm, perr := processCase(opts.Out, c)
		if perr != nil {
			return nil, perr
		}
		suite.CasesManifests = append(suite.CasesManifests, *cm)
		suite.Clean += boolInt(cm.Class == fixture.ClassClean)
		suite.Repaired += boolInt(cm.Class == fixture.ClassRepaired)
		suite.Rejected += boolInt(cm.Class == fixture.ClassRejected)
		suite.CCGaps += cm.CCGaps
		suite.PSISwitches += cm.PSISwitches
		suite.PCRWraps += cm.PCRWraps
		suite.LateReplays += cm.LateReplays
		suite.RTPDuplicates += cm.RTPDuplicates
		suite.RTPOutsideWindow += cm.RTPOutsideWindow
		suite.SampleHashMatched += cm.HashMatched
		suite.SampleHashTotal += cm.HashTotal
		if cm.SkewMS > suite.ManifestSkewMS {
			suite.ManifestSkewMS = cm.SkewMS
		}
		if !cm.ReplayHashEqual {
			suite.ReplayHashEqual = false
		}
	}

	acceptedTarget, acceptedActual, bitrateCases := int64(0), int64(0), int64(0)
	for _, cm := range suite.CasesManifests {
		if cm.Class == fixture.ClassRejected {
			continue
		}
		acceptedTarget += cm.TargetBitrateBPS
		acceptedActual += cm.ActualBitrateBPS
		bitrateCases++
	}
	if bitrateCases > 0 {
		suite.BitrateTargetBPS = acceptedTarget / bitrateCases
		suite.BitrateActualBPS = acceptedActual / bitrateCases
	}
	suite.BitrateDeltaPPM = audit.PPM(suite.BitrateTargetBPS, suite.BitrateActualBPS)
	if suite.SampleHashTotal > 0 {
		suite.SampleHashMatch = audit.FormatPercent(suite.SampleHashMatched, suite.SampleHashTotal)
	}
	if err := checkpoint.WriteJSONAtomic(filepath.Join(opts.Out, "suite-manifest.json"), suite); err != nil {
		return nil, err
	}
	return &suite, nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
