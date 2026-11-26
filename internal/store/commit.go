package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"pairflow/internal/media"
)

// Layout paths inside a task directory.
const (
	StagingDir = "staging"
	OutDir     = "out"
	Manifest   = "manifest.json"
)

// Manifest records the committed state of a run: the timeline identity and the
// ordered stage artifacts. It is itself the last atomically committed file.
type ManifestDoc struct {
	Version   int        `json:"version"`
	Timeline  string     `json:"timeline_sha256"`
	Seed      uint64     `json:"seed"`
	Stages    []StageDoc `json:"stages"`
	Faults    string     `json:"faults"`
	CreatedAt string     `json:"created_at"`
}

// StageDoc is one committed stage and its artifacts.
type StageDoc struct {
	Name      string     `json:"stage"`
	Artifacts []Artifact `json:"artifacts"`
}

// StageStager owns the staging area for one stage. Files appear under
// staging/<stage>/ and are synced before the stage may commit.
type StageStager struct {
	jail  *Jail
	stage string
	arts  []Artifact
}

// NewStageStager clears any leftover staging directory for the stage.
func NewStageStager(j *Jail, stage string) (*StageStager, *media.Error) {
	dir := filepath.Join(StagingDir, stage)
	abs, err := j.EnsureDir(dir)
	if err != nil {
		return nil, err
	}
	// Remove any partial leftovers from an earlier interrupted attempt.
	entries, derr := os.ReadDir(abs)
	if derr != nil {
		return nil, media.Wrap(derr, media.ClassInternal, "store.stage_list", "cannot list staging area")
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if rerr := os.Remove(filepath.Join(abs, e.Name())); rerr != nil {
			return nil, media.Wrap(rerr, media.ClassInternal, "store.stage_clear", "cannot clear stale staging file")
		}
	}
	return &StageStager{jail: j, stage: stage}, nil
}

// WriteStaged writes bytes to staging/<stage>/<name>, fsyncs the file and
// records its hash. Artifacts are not yet visible under out/.
func (s *StageStager) WriteStaged(name string, data []byte, kind string) (Artifact, *media.Error) {
	dir := filepath.Join(StagingDir, s.stage)
	rel := filepath.Join(dir, name)
	abs, err := s.jail.Resolve(rel)
	if err != nil {
		return Artifact{}, err
	}
	if werr := os.WriteFile(abs, data, 0o644); werr != nil {
		return Artifact{}, media.Wrap(werr, media.ClassInternal, "store.stage_write", "cannot write staged artifact").WithPath(rel)
	}
	if _, serr := s.jail.SyncFile(rel); serr != nil {
		return Artifact{}, serr
	}
	a := Artifact{Path: filepath.Join(OutDir, s.stage, name), Size: int64(len(data)), SHA: HashBytes(data), Kind: kind}
	s.arts = append(s.arts, a)
	return a, nil
}

// Artifacts returns the staged artifacts recorded so far.
func (s *StageStager) Artifacts() []Artifact { return append([]Artifact(nil), s.arts...) }

// Commit atomically renames every staged file into out/<stage>/, syncing the
// destination directories. Same-volume rename guarantees readers see either
// the old state or the complete new file, never a partial one.
func (s *StageStager) Commit() ([]Artifact, *media.Error) {
	if len(s.arts) == 0 {
		return nil, media.E(media.ClassInternal, "store.nothing_staged", "stage %q staged no artifacts", s.stage)
	}
	dstDir := filepath.Join(OutDir, s.stage)
	if _, err := s.jail.EnsureDir(dstDir); err != nil {
		return nil, err
	}
	for i := range s.arts {
		a := &s.arts[i]
		name := filepath.Base(a.Path)
		src, err := s.jail.Resolve(filepath.Join(StagingDir, s.stage, name))
		if err != nil {
			return nil, err
		}
		dst, err := s.jail.Resolve(a.Path)
		if err != nil {
			return nil, err
		}
		if rerr := os.Rename(src, dst); rerr != nil {
			return nil, media.Wrap(rerr, media.ClassInternal, "store.rename", "cannot commit staged artifact").WithPath(a.Path)
		}
		if err := SyncDir(filepath.Dir(dst)); err != nil {
			return nil, err
		}
	}
	// Remove the now-empty staging directory so a crash cannot confuse it
	// with pending work.
	stageDir, err := s.jail.Resolve(filepath.Join(StagingDir, s.stage))
	if err == nil {
		_ = os.Remove(stageDir)
	}
	return s.Artifacts(), nil
}

// Rollback deletes staging and any partially committed out/<stage> files for
// this stage, then syncs the parent directories. It never touches other
// stages' committed artifacts.
func (s *StageStager) Rollback() *media.Error {
	stageDir := filepath.Join(StagingDir, s.stage)
	abs, err := s.jail.Resolve(stageDir)
	if err != nil {
		return err
	}
	if rerr := os.RemoveAll(abs); rerr != nil {
		return media.Wrap(rerr, media.ClassIntegrity, "store.rollback_staging", "cannot remove staging").WithPath(stageDir)
	}
	outStage := filepath.Join(OutDir, s.stage)
	outAbs, rerr := s.jail.Resolve(outStage)
	if rerr == nil {
		if derr := os.RemoveAll(outAbs); derr != nil {
			return media.Wrap(derr, media.ClassIntegrity, "store.rollback_out", "cannot remove uncommitted output").WithPath(outStage)
		}
		_ = SyncDir(filepath.Join(s.jail.Root(), OutDir))
	}
	return nil
}

// ReadManifest loads the run manifest, if present.
func (j *Jail) ReadManifest() (*ManifestDoc, bool, *media.Error) {
	data, err := j.ReadFile(Manifest)
	if err != nil {
		if errors.Is(unwrapCause(err), fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var m ManifestDoc
	if uerr := json.Unmarshal(data, &m); uerr != nil {
		return nil, false, media.Wrap(uerr, media.ClassIntegrity, "store.manifest_corrupt", "manifest is not valid JSON").WithPath(Manifest)
	}
	return &m, true, nil
}

// WriteManifest atomically commits the manifest via staging plus rename.
func (j *Jail) WriteManifest(m *ManifestDoc) *media.Error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return media.Wrap(err, media.ClassInternal, "store.manifest_encode", "cannot encode manifest")
	}
	tmpRel := filepath.Join(StagingDir, "manifest.partial")
	absTmp, rerr := j.Resolve(tmpRel)
	if rerr != nil {
		return rerr
	}
	if _, err := j.EnsureDir(StagingDir); err != nil {
		return err
	}
	if werr := os.WriteFile(absTmp, append(data, '\n'), 0o644); werr != nil {
		return media.Wrap(werr, media.ClassInternal, "store.manifest_write", "cannot write manifest").WithPath(tmpRel)
	}
	if _, err := j.SyncFile(tmpRel); err != nil {
		return err
	}
	dst, derr := j.Resolve(Manifest)
	if derr != nil {
		return derr
	}
	if rerr := os.Rename(absTmp, dst); rerr != nil {
		return media.Wrap(rerr, media.ClassInternal, "store.manifest_rename", "cannot commit manifest").WithPath(Manifest)
	}
	return SyncDir(j.root)
}

// unwrapCause exposes the root cause of a wrapped error for ErrNotExist
// checks, since the jaw wraps os errors with a media.Error cause.
func unwrapCause(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		next := u.Unwrap()
		if next == nil {
			return err
		}
		err = next
	}
}

// SortedStages orders stage docs by name for deterministic manifests.
func SortedStages(stages []StageDoc) []StageDoc {
	out := append([]StageDoc(nil), stages...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
