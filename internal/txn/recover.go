package txn

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mediarig/internal/media"
)

// Recovery is the verified state loaded by resume.
type Recovery struct {
	Paths        *Paths
	Params       Params
	ParamsDigest string
	InputDigest  string
	Records      []Record
	Checkpoints  map[string]Record
	Committed    bool
	TailRepaired bool
	RemovedTemps []string
}

// LoadAndVerify repairs the WAL tail, reads persisted parameters, verifies
// input/parameter digests, removes uncommitted temp files, and verifies all
// artifacts referenced by committed checkpoints.
func LoadAndVerify(root string) (*Recovery, error) {
	p, err := Layout(root)
	if err != nil {
		return nil, err
	}
	var params Params
	if err := ReadJSON(p.Params, &params); err != nil {
		return nil, err
	}
	params.Root = p.Root
	pd, err := params.Digest()
	if err != nil {
		return nil, err
	}
	rawParams, err := os.ReadFile(p.Params)
	if err != nil {
		return nil, media.InternalWrap(media.StageRecover, err, "read params bytes")
	}
	// Params JSON includes Root which may be absolute and was persisted;
	// digest was computed before persistence with same Root set by run.
	_ = rawParams

	inputDigest, _, err := SHA256File(p.InputFile)
	if err != nil {
		return nil, err
	}
	records, err := ReadAndRepair(p.WAL)
	if err != nil {
		return nil, err
	}
	checkpoints := map[string]Record{}
	for _, r := range records {
		if r.Status == "checkpoint" {
			checkpoints[r.Stage] = r
		}
	}
	rec := &Recovery{
		Paths:        p,
		Params:       params,
		ParamsDigest: pd,
		InputDigest:  inputDigest,
		Records:      records,
		Checkpoints:  checkpoints,
	}
	if _, err := os.Stat(p.Committed); err == nil {
		rec.Committed = true
	} else if !os.IsNotExist(err) {
		return nil, media.InternalWrap(media.StageRecover, err, "stat commit marker")
	}

	for _, r := range records {
		if r.InputSHA256 != "" && r.InputSHA256 != inputDigest {
			return nil, media.Corrupt(media.StageRecover, "input digest differs from WAL record %d", r.Seq)
		}
		if r.ParamsSHA256 != "" && r.ParamsSHA256 != pd {
			// Run persists params including absolute root and computes digest
			// from the same struct; this branch catches tampering.
			return nil, media.Corrupt(media.StageRecover, "parameter digest differs from WAL record %d", r.Seq)
		}
		for _, a := range r.Artifacts {
			if err := verifyArtifact(a); err != nil {
				return nil, err
			}
		}
	}
	removed, err := CleanTemporaryFiles(p)
	if err != nil {
		return nil, err
	}
	rec.RemovedTemps = removed
	return rec, nil
}

func verifyArtifact(a Artifact) error {
	if a.SHA256 == "" {
		return media.Corrupt(media.StageRecover, "WAL artifact %s lacks checksum", a.Name)
	}
	d, n, err := SHA256File(a.Path)
	if err != nil {
		return err
	}
	if n != a.Size || d != a.SHA256 {
		return media.Corrupt(media.StageRecover, "committed artifact %s verification failed", a.Name)
	}
	return nil
}

// CleanTemporaryFiles removes only dot-temp files created by WriteAtomic or
// CommitOpenFile. Final stage artifacts and committed outputs are retained.
func CleanTemporaryFiles(p *Paths) ([]string, error) {
	dirs := []string{p.Root}
	_ = filepath.Walk(p.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if path != p.Root {
				dirs = append(dirs, path)
			}
			return nil
		}
		return nil
	})
	var removed []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, media.InternalWrap(media.StageRecover, err, "read directory %s", dir)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasPrefix(name, ".tmp-") {
				full := filepath.Join(dir, name)
				if err := os.Remove(full); err != nil {
					return nil, media.InternalWrap(media.StageRecover, err, "remove temp %s", full)
				}
				removed = append(removed, full)
			}
		}
	}
	sort.Strings(removed)
	return removed, nil
}

// ResetForFreshRun removes prior transaction products while preserving the
// input directory. Resume never calls this; it retains and verifies WAL state.
func ResetForFreshRun(p *Paths) error {
	files := []string{
		p.WAL, p.Committed, p.Params, p.CaptureJSON, p.AudioPCM, p.AudioJSON,
		p.ColorJSON, p.MP4JSON, p.MP4, p.Manifest, p.Stats, p.ErrorJSON,
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return media.InternalWrap(media.StageInput, err, "reset %s", f)
		}
	}
	if _, err := CleanTemporaryFiles(p); err != nil {
		return err
	}
	return SyncDir(p.Root)
}

// StageDone reports whether a stage has a verified checkpoint.
func (r *Recovery) StageDone(stage media.Stage) bool {
	_, ok := r.Checkpoints[string(stage)]
	return ok
}

var batchSlots = make(chan struct{}, MaxParallelBatches)

// AcquireBatch enforces the four-parallel-batch policy. It waits until a slot
// is available and returns the release function.
func AcquireBatch() func() {
	batchSlots <- struct{}{}
	return func() { <-batchSlots }
}
