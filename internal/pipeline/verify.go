package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mediascan/internal/index"
	"mediascan/internal/recover"
	"mediascan/internal/report"
)

// Verify exit codes, matching the CLI contract.
const (
	VerifyOK       = 0
	VerifyUsage    = 1
	VerifyMismatch = 2
)

// Verify re-derives every digest from the inputs and compares it with the
// recorded checkpoint and provenance. It returns a process exit code and a
// human-readable message. A digest mismatch is code 2; a missing or
// unreadable work dir is code 1.
func Verify(o Options) (int, string) {
	o.Defaults()
	paths := index.NewPaths(o.Work)
	idx, err := index.ReadIndex(paths.Index)
	if err != nil {
		return VerifyUsage, fmt.Sprintf("cannot read index: %v", err)
	}
	store := recover.NewStore(o.Work)
	cp, err := store.Load()
	if err != nil {
		return VerifyMismatch, fmt.Sprintf("cannot read checkpoint: %v", err)
	}
	if cp == nil {
		return VerifyMismatch, "no checkpoint found"
	}
	// Analysis must run with the parameters the checkpoint was bound to,
	// otherwise a legitimate rerun would appear to disagree.
	o.AVTolMS = cp.Params.AVToleranceMS
	o.MaxGOPMS = cp.Params.MaxGOPMS
	o.LossEvery = cp.Params.LossEvery
	if _, err := o.Params().Normalize(); err != nil {
		return VerifyUsage, fmt.Sprintf("checkpoint parameters invalid: %v", err)
	}
	// Re-scan from scratch: verify never trusts persisted hashes.
	res, primary, secondary, err := Load(o)
	if err != nil {
		return VerifyUsage, fmt.Sprintf("cannot load manifests: %v", err)
	}
	fresh, err := index.Scan(res, primary, index.Options{})
	if err != nil {
		return VerifyUsage, fmt.Sprintf("rescan failed: %v", err)
	}
	if fresh.InputSHA != idx.InputSHA {
		return VerifyMismatch, fmt.Sprintf("input_sha256 mismatch: index=%s current=%s", idx.InputSHA, fresh.InputSHA)
	}
	if cp.ManifestSHA != fresh.InputSHA {
		return VerifyMismatch, fmt.Sprintf("checkpoint bound to %s but inputs hash to %s", cp.ManifestSHA, fresh.InputSHA)
	}
	for i, entry := range fresh.Entries {
		if i >= len(idx.Entries) {
			return VerifyMismatch, fmt.Sprintf("index has %d entries, inputs now yield %d", len(idx.Entries), len(fresh.Entries))
		}
		if entry.SHA256 != idx.Entries[i].SHA256 {
			return VerifyMismatch, fmt.Sprintf("segment %s digest changed", entry.Name)
		}
	}
	if len(fresh.Entries) != len(idx.Entries) {
		return VerifyMismatch, fmt.Sprintf("entry count changed: %d != %d", len(idx.Entries), len(fresh.Entries))
	}
	// Re-run the analysis phase against the freshly scanned index and compare
	// the resulting digest with the checkpoint.
	result, err := BuildResult(res, primary, secondary, fresh, o)
	if err != nil {
		return VerifyUsage, fmt.Sprintf("analysis failed: %v", err)
	}
	if cp.ResultSHA == "" {
		return VerifyMismatch, "checkpoint has no recorded result digest"
	}
	if cp.ResultSHA != result.ResultSHA {
		return VerifyMismatch, fmt.Sprintf("result_sha256 mismatch: checkpoint=%s current=%s", cp.ResultSHA, result.ResultSHA)
	}
	provPath := filepath.Join(o.Work, "report", "provenance.jsonl")
	if _, statErr := os.Stat(provPath); statErr == nil {
		if err := report.VerifyResult(provPath, result.InputSHA, result.ResultSHA); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return VerifyUsage, err.Error()
			}
			return VerifyMismatch, err.Error()
		}
	}
	return VerifyOK, fmt.Sprintf("input_sha256=%s result_sha256=%s segments=%d checkpoint=%d",
		result.InputSHA, result.ResultSHA, result.Segments, cp.NextSeg)
}
