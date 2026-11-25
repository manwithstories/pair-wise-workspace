package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"avcheck/internal/ebml"
	"avcheck/internal/media"
	"avcheck/internal/report"
)

// RunFromBytes analyses an in-memory container and writes output into outDir.
// A truncated document commits diagnostics only (never a manifest) and
// reports Committed=false.
func (r *Runner) RunFromBytes(ctx context.Context, data []byte, outDir string) (*RunResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, &CanceledError{Err: err}
	}
	deadline := r.cfg.Deadline
	if deadline == 0 {
		deadline = DefaultDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	src := newCancelReader(ctx, newByteSource(data))
	doc, err := ebml.Parse(src, r.cfg.Limits)
	if err != nil {
		if isCanceled(err) {
			return nil, &CanceledError{Err: err}
		}
		var fs *media.Faults
		if errors.As(err, &fs) {
			r.merge(fs)
			return r.commitDiagnostics(outDir)
		}
		return nil, err
	}
	r.merge(doc.Faults)
	graph, err := ebml.NewGraph(doc)
	if err != nil {
		// The graph stage records its own faults on the document, so fold
		// them in before committing diagnostics.
		r.merge(doc.Faults)
		return r.commitDiagnostics(outDir)
	}
	r.merge(doc.Faults)
	if doc.Truncated() {
		return r.commitDiagnostics(outDir)
	}
	res, err := r.align(ctx, doc, graph)
	if err != nil {
		if isCanceled(err) {
			return nil, &CanceledError{Err: err}
		}
		var fs *media.Faults
		if errors.As(err, &fs) {
			return r.commitDiagnostics(outDir)
		}
		return nil, err
	}
	m, err := report.Build(res, report.Options{
		TargetBps:  r.cfg.TargetBps,
		DurationNs: doc.DurationNs,
		Seed:       r.cfg.Seed,
		MemoryCap:  r.cfg.MemoryBytes,
	})
	if err != nil {
		return nil, err
	}
	if r.fault.MaxLevel() == media.LevelAbort {
		return r.commitDiagnostics(outDir)
	}
	path, err := report.WriteAtomic(outDir, m)
	if err != nil {
		return nil, err
	}
	return &RunResult{
		Manifest: m, Faults: r.fault, Committed: true,
		ManifestPath: path, OutputBytes: res.PayloadBytes,
	}, nil
}

// merge folds faults produced by a stage into the runner's model so later
// stages and the final report see one ordered list.
func (r *Runner) merge(fs *media.Faults) {
	if fs == nil {
		return
	}
	for _, f := range fs.List {
		if !containsFault(r.fault, f) {
			r.fault.Add(f)
		}
	}
}

func containsFault(fs *media.Faults, f media.Fault) bool {
	for _, g := range fs.List {
		if g == f {
			return true
		}
	}
	return false
}

// commitDiagnostics writes only diagnostics.txt, never a manifest, so a
// damaged input is inspectable without pretending the output is final.
func (r *Runner) commitDiagnostics(outDir string) (*RunResult, error) {
	if len(r.fault.List) == 0 {
		r.fault.Addf(media.LevelAbort, media.CodeInternal, 0, 0, "document could not be processed")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	name, err := writeFileAtomic(outDir, "diagnostics.txt", []byte(report.Diagnostics(r.fault)))
	if err != nil {
		return nil, err
	}
	return &RunResult{Faults: r.fault, Committed: false, ManifestPath: name}, nil
}

// writeFileAtomic writes body to outDir/name through a temp file and rename.
func writeFileAtomic(outDir, name string, body []byte) (string, error) {
	tmp, err := os.CreateTemp(outDir, "."+name+"-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	final := filepath.Join(outDir, name)
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return final, nil
}
