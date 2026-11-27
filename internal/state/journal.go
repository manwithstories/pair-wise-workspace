package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"avcore/internal/adapter"
)

// Fingerprint binds a run to its exact inputs and toolchain, so a change to
// source content, configuration or adapter version invalidates every old
// checkpoint instead of silently mixing incompatible segments.
type Fingerprint struct {
	InputHash      string `json:"input_hash"`
	ConfigHash     string `json:"config_hash"`
	AdapterVersion string `json:"adapter_version"`
}

// SegmentRecord is one committed, hash-verified segment.
type SegmentRecord struct {
	ID            string `json:"id"`
	Variant       int    `json:"variant"`
	CutIndex      int    `json:"cut_index"`
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	Bytes         int64  `json:"bytes"`
	StartTicks    int64  `json:"start_ticks"`
	EndTicks      int64  `json:"end_ticks"`
	TargetBPS     int    `json:"target_bps"`
	ActualBPS     int    `json:"actual_bps"`
	PSNRMeanDB    float64 `json:"psnr_mean_db"`
	SSIMMean      float64 `json:"ssim_mean"`
}

// CaptionRecord commits a generated per-cut caption playlist fragment.
type CaptionRecord struct {
	CutIndex int    `json:"cut_index"`
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
}

// Journal is the on-disk resumable state for one job.
type Journal struct {
	JobDir     string          `json:"job_dir"`
	Fingerprint Fingerprint    `json:"fingerprint"`
	Segments   []SegmentRecord `json:"segments"`
	Captions   []CaptionRecord `json:"captions"`
	// Reused counts checkpoints accepted on recovery (audit field).
	Reused int `json:"reused"`
	path   string
}

const journalName = "journal.json"

// Open loads an existing journal or initializes a fresh one in jobDir.
func Open(jobDir string, fp Fingerprint) (*Journal, error) {
	if err := os.MkdirAll(filepath.Join(jobDir, "tmp"), 0o755); err != nil {
		return nil, adapter.NewError(adapter.CodeInternal, "cannot create job dir: "+err.Error(),
			adapter.Source{File: jobDir})
	}
	j := &Journal{
		JobDir:      jobDir,
		Fingerprint: fp,
		Segments:    []SegmentRecord{},
		Captions:    []CaptionRecord{},
		path:        filepath.Join(jobDir, journalName),
	}
	raw, err := os.ReadFile(j.path)
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return nil, adapter.NewError(adapter.CodeInternal, "journal read: "+err.Error(),
			adapter.Source{File: j.path})
	}
	var prior Journal
	if jerr := json.Unmarshal(raw, &prior); jerr != nil {
		return nil, adapter.NewError(adapter.CodeHashMismatch, "corrupt journal: "+jerr.Error(),
			adapter.Source{File: j.path})
	}
	if prior.Fingerprint != fp {
		// A different input/config/toolchain: start clean rather than mix.
		return j, nil
	}
	prior.path = j.path
	prior.JobDir = jobDir
	return &prior, nil
}

// TempPath returns a staging path inside the job's tmp directory.
func (j *Journal) TempPath(name string) string {
	return filepath.Join(j.JobDir, "tmp", name+".part")
}

// FinalPath returns the committed location for a segment name.
func (j *Journal) FinalPath(name string) string {
	return filepath.Join(j.JobDir, name)
}

// Commit verifies the staged file's hash against want and atomically moves it
// into place, recording the checkpoint. A nil want means "record the observed
// hash" (first commit). The move is atomic within the same directory.
func (j *Journal) Commit(rec SegmentRecord, want string) (*SegmentRecord, error) {
	tmp := filepath.Join(j.JobDir, "tmp", filepath.Base(rec.Path)+".part")
	got, err := FileSHA256(tmp)
	if err != nil {
		return nil, adapter.NewError(adapter.CodeHashMismatch, "cannot hash temp segment: "+err.Error(),
			adapter.Source{File: tmp})
	}
	if want != "" && got != want {
		_ = os.Remove(tmp)
		return nil, adapter.NewError(adapter.CodeHashMismatch,
			fmt.Sprintf("segment hash mismatch: want %s got %s", want, got),
			adapter.Source{File: rec.Path})
	}
	rec.SHA256 = got
	final := filepath.Join(j.JobDir, filepath.Base(rec.Path))
	if err := os.Rename(tmp, final); err != nil {
		return nil, adapter.NewError(adapter.CodeInternal, "atomic rename failed: "+err.Error(),
			adapter.Source{File: final})
	}
	info, _ := os.Stat(final)
	if info != nil {
		rec.Bytes = info.Size()
	}
	rec.Path = filepath.Base(final)
	j.Segments = append(j.Segments, rec)
	if err := j.flush(); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Lookup returns a previously committed segment for a variant/cut pair.
func (j *Journal) Lookup(variant, cut int) (SegmentRecord, bool) {
	for _, s := range j.Segments {
		if s.Variant == variant && s.CutIndex == cut {
			return s, true
		}
	}
	return SegmentRecord{}, false
}

// Verify re-validates every checkpoint against its referenced file. Valid
// entries are kept and counted as reused; missing/hash-broken entries are
// dropped so the caller re-encodes them. It never trusts a stale record.
func (j *Journal) Verify() (int, []adapter.Error) {
	kept := j.Segments[:0]
	var faults []adapter.Error
	for _, s := range j.Segments {
		full := filepath.Join(j.JobDir, filepath.Base(s.Path))
		got, err := FileSHA256(full)
		if err != nil {
			faults = append(faults, *adapter.NewError(adapter.CodeMissingSegment,
				"checkpoint file missing: "+err.Error(), adapter.Source{File: s.Path}))
			continue
		}
		if got != s.SHA256 {
			faults = append(faults, *adapter.NewError(adapter.CodeHashMismatch,
				"checkpoint hash mismatch", adapter.Source{File: s.Path}))
			_ = os.Remove(full)
			continue
		}
		kept = append(kept, s)
	}
	j.Segments = kept
	j.Reused = len(kept)

	cc := j.Captions[:0]
	for _, c := range j.Captions {
		full := filepath.Join(j.JobDir, filepath.Base(c.Path))
		got, err := FileSHA256(full)
		if err != nil || got != c.SHA256 {
			faults = append(faults, *adapter.NewError(adapter.CodeHashMismatch,
				"caption checkpoint invalid", adapter.Source{File: c.Path}))
			continue
		}
		cc = append(cc, c)
	}
	j.Captions = cc
	return j.Reused, faults
}

// Flush persists the current journal state atomically. It is needed when
// caption records are appended outside of Commit.
func (j *Journal) Flush() error { return j.flush() }

// flush atomically writes the journal via a temp file and rename.
func (j *Journal) flush() error {
	j.sortSegments()
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return adapter.NewError(adapter.CodeInternal, "journal marshal: "+err.Error(), adapter.Source{})
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return adapter.NewError(adapter.CodeInternal, "journal write: "+err.Error(),
			adapter.Source{File: tmp})
	}
	if err := os.Rename(tmp, j.path); err != nil {
		return adapter.NewError(adapter.CodeInternal, "journal rename: "+err.Error(),
			adapter.Source{File: j.path})
	}
	return nil
}

func (j *Journal) sortSegments() {
	sort.SliceStable(j.Segments, func(a, b int) bool {
		if j.Segments[a].Variant != j.Segments[b].Variant {
			return j.Segments[a].Variant < j.Segments[b].Variant
		}
		return j.Segments[a].CutIndex < j.Segments[b].CutIndex
	})
}
