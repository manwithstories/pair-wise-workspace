// Package engine orchestrates the full mediaroute pipeline: container
// scanning with reusable indexes, fault-injected RTP streaming, check-
// pointed recovery and machine-readable reporting.
package engine

import (
	"strings"

	"mediaroute/internal/fault"
	"mediaroute/internal/index"
	"mediaroute/internal/probe"
)

// ScanConfig controls one scan-or-reuse cycle.
type ScanConfig struct {
	InputPath string
	Format    string // "mp4","mxf" or "" for extension-based auto
	IndexDir  string
	Params    index.Params
}

// ScanResult reports what the scan stage did.
type ScanResult struct {
	Report   *probe.ContainerReport
	Reused   bool         // true when a valid index was loaded
	IndexErr *fault.Error // non-nil when an existing index was rejected
}

// Scan runs the scan stage: it first attempts to reuse a valid index and
// falls back to a fresh container scan when the index is missing or
// invalid. A rejected index never aborts the run; the fresh report is
// re-indexed atomically.
func Scan(cfg ScanConfig) (*ScanResult, error) {
	if cfg.InputPath == "" {
		return nil, fault.New(fault.CatBadArgument, "empty input path")
	}
	format := cfg.Format
	if format == "" {
		format = autoFormat(cfg.InputPath)
	}
	store := index.NewStore(cfg.IndexDir, cfg.Params)
	// The input digest is cheap for the bounded local files we accept and
	// is required before any fingerprint comparison.
	digest, err := digestFile(cfg.InputPath)
	if err != nil {
		return nil, err
	}
	res := &ScanResult{}
	if rep, _, lerr := store.Load(cfg.InputPath, digest); lerr == nil {
		res.Report, res.Reused = rep, true
		return res, nil
	} else {
		res.IndexErr = fault.AsError(lerr)
	}
	rep, err := scanByFormat(format, cfg.InputPath)
	if err != nil {
		return nil, err
	}
	// A freshly scanned file must match the digest we computed up front;
	// any drift means the file changed underneath us.
	if rep.InputSHA256 != digest {
		return nil, fault.New(fault.CatIndexInvalid,
			"input digest changed during scan")
	}
	if err := store.Invalidate(cfg.InputPath); err != nil {
		return nil, err
	}
	if err := store.Save(cfg.InputPath, rep); err != nil {
		return nil, err
	}
	res.Report = rep
	return res, nil
}

func autoFormat(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.HasSuffix(p, ".mp4") || strings.HasSuffix(p, ".mov"):
		return probe.FormatMP4
	case strings.HasSuffix(p, ".mxf"):
		return probe.FormatMXF
	default:
		return probe.FormatMP4
	}
}

func scanByFormat(format, path string) (*probe.ContainerReport, error) {
	switch format {
	case probe.FormatMP4:
		return probe.ScanMP4(path)
	case probe.FormatMXF:
		return probe.ScanMXF(path)
	default:
		return nil, fault.New(fault.CatBadArgument,
			"unsupported format %q", format)
	}
}
