// Package pipeline wires scanning, clock repair, indexing, metrics and the
// transactional commit together.
package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"bcast/internal/boxes"
	"bcast/internal/clock"
	"bcast/internal/index"
	"bcast/internal/media"
	"bcast/internal/metrics"
	"bcast/internal/txn"
)

// ErrNoInterruptedGeneration is returned by Resume when there is no COMMIT
// marker and no staging generation to recover.
var ErrNoInterruptedGeneration = errors.New("no interrupted staging generation")

// Generation is the deterministic demo generation.  Fixed naming makes resume
// and repeated SHA-256 checks simple and reproducible.
const Generation = "gen-0001"

// Options configures one pipeline execution.
type Options struct {
	Out        string
	SourceDir  string
	Generation string
	FailAfter  string
	TargetKBPS int
}

// Run executes a fresh or resumed deterministic batch.
func Run(opts Options) (*metrics.Report, error) {
	gen := opts.Generation
	if gen == "" {
		gen = Generation
	}
	source := opts.SourceDir
	if source == "" {
		source = filepath.Join(opts.Out, "input")
	}
	target := opts.TargetKBPS
	if target == 0 {
		target = 6000
	}

	if rec, err, ok := txn.LoadCommit(opts.Out); err != nil {
		return nil, err
	} else if ok {
		return loadCommitted(opts.Out, rec, 0)
	}
	gens, _ := txn.StagingGenerations(opts.Out)
	recovered := len(gens)

	t, err := txn.Begin(opts.Out, gen)
	if err != nil {
		return nil, err
	}
	defer t.Close()

	base := &metrics.Report{TargetKBPS: target, Recovered: recovered}
	scan, err := boxes.ScanBatch(source, boxes.ScanOptions{Concurrency: 4})
	if err != nil {
		return abortCorrupt(t, opts.Out, base, "isolated", err)
	}
	base.MissingCount = scan.Sequences.MissingCount()
	base.DuplicateCount = scan.Sequences.DuplicateCount()
	base.OutOfOrderCount = len(scan.Sequences.OutOfOrder)
	base.IsolatedCount = len(scan.Isolated)
	_ = t.Log("scan", "ok", "unique="+itoa(len(scan.Unique)))
	if opts.FailAfter == "scan" {
		return base, media.Interrupted("scan")
	}

	anchors, err := clock.LoadAnchors(source)
	if err != nil {
		return abortCorrupt(t, opts.Out, base, "blocked", err)
	}
	timeline, err := clock.Repair(scan, anchors, clock.RepairOptions{AllowAnchorlessClamp: true})
	if err != nil {
		return abortCorrupt(t, opts.Out, base, "blocked", err)
	}
	_ = t.Log("repair", "ok", "tracks="+itoa(len(timeline.Tracks)))
	if opts.FailAfter == "repair" {
		return base, media.Interrupted("repair")
	}

	art, err := index.Build(timeline)
	if err != nil {
		return abortCorrupt(t, opts.Out, base, "blocked", err)
	}
	files := []struct {
		name string
		data []byte
	}{
		{index.ManifestFile, art.Manifest},
		{index.IndexJSONFile, art.IndexJSON},
		{index.SIDXFile, art.SIDX},
		{index.TFRAFile, art.TFRA},
	}
	for _, f := range files {
		if err := t.WriteArtifact(f.name, f.data); err != nil {
			return nil, err
		}
	}
	_ = t.Log("index", "ok", "entries="+itoa(len(art.Entries)))
	if opts.FailAfter == "index" {
		return base, media.Interrupted("index")
	}

	verify, err := index.Verify(source, art)
	if err != nil {
		return abortCorrupt(t, opts.Out, base, "blocked", err)
	}
	report := metrics.Summarize(scan, timeline, target)
	report.Recovered = recovered
	report.SetSeekVerification(verify.SeekOK, verify.SeekTotal, verify.ManifestSkew)

	reportJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, media.Corrupt("metrics", "encode_report", index.ReportFile, 0, -1, "%v", err)
	}
	reportJSON = append(reportJSON, '\n')
	if err := t.WriteArtifact(index.ReportFile, reportJSON); err != nil {
		return nil, err
	}
	_ = t.Log("metrics", "ok", "samples="+itoa(report.SampleCount))
	if opts.FailAfter == "metrics" {
		return report, media.Interrupted("metrics")
	}

	rec, err := t.Commit()
	if err != nil {
		return nil, err
	}
	report.SHA256 = rec.SHA256
	report.Committed = 1
	report.Generation = gen
	return report, nil
}

// Resume recovers an interrupted generation or returns the existing commit.
func Resume(out string) (*metrics.Report, error) {
	rec, err, ok := txn.LoadCommit(out)
	if err != nil {
		return nil, err
	}
	if ok {
		return loadCommitted(out, rec, 0)
	}
	gens, err := txn.StagingGenerations(out)
	if err != nil {
		return nil, err
	}
	if len(gens) == 0 {
		return nil, media.IO("resume", out, 0, -1, ErrNoInterruptedGeneration)
	}
	return Run(Options{Out: out, Generation: gens[0]})
}

func loadCommitted(out string, rec *txn.CommitRecord, recovered int) (*metrics.Report, error) {
	path := filepath.Join(out, "committed", rec.Generation, index.ReportFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, media.Corrupt("resume", "missing_committed_report", path, 0, -1, "%v", err)
		}
		return nil, media.IO("resume", path, 0, 0, err)
	}
	var r metrics.Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, media.Corrupt("resume", "invalid_report", path, 0, -1, "%v", err)
	}
	r.SHA256 = rec.SHA256
	r.Committed = 1
	r.Recovered = recovered
	r.Generation = rec.Generation
	return &r, nil
}

func abortCorrupt(t *txn.Txn, out string, r *metrics.Report, status string, cause error) (*metrics.Report, error) {
	_ = t.Close()
	n, rbErr := txn.RollbackStaging(out)
	if rbErr != nil {
		return nil, rbErr
	}
	r.Status = status
	r.Rollback = n
	if me, ok := media.AsError(cause); ok && me.Offset >= 0 {
		r.FirstErrorByte = me.Offset
	}
	return r, cause
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
