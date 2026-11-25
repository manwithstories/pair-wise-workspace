package pipeline

import (
	"path/filepath"

	"mediad/internal/fixture"
	"mediad/internal/resume"
)

// PublishReport counts newly committed and idempotently replayed artifacts.
type PublishReport struct {
	Committed int
	Replayed  int
}

// Publish atomically commits initialization and segment artifacts, records
// their hashes in the checkpoint, and optionally publishes HLS playlists.
// exclusiveSegments limits segment indices to i < exclusiveSegments; a negative
// value publishes every aligned segment.
func Publish(dir string, specs []fixture.RenditionSpec, b *Built, cp *resume.Checkpoint, exclusiveSegments int, writeManifest bool) (*PublishReport, error) {
	rep := &PublishReport{}
	checkpointPath := filepath.Join(dir, "checkpoint.json")
	commit := func(key, rel string, data []byte) error {
		replayed, err := cp.CommitOutput(dir, key, rel, data)
		if err != nil {
			return err
		}
		if replayed {
			rep.Replayed++
			return nil
		}
		rep.Committed++
		return cp.Save(checkpointPath)
	}

	for r, spec := range specs {
		if err := commit("init:"+spec.ID, InitRelPath(spec), b.Init); err != nil {
			return nil, err
		}
		limit := exclusiveSegments
		if limit < 0 || limit > len(b.Alignment.Segments) {
			limit = len(b.Alignment.Segments)
		}
		for i := 0; i < limit; i++ {
			key := packageKey(r, i)
			pkg := b.Packages[key]
			if pkg == nil {
				return nil, errMissingPackage(r, i)
			}
			if err := commit(key, PackageRelPath(spec, i), pkg.Media); err != nil {
				return nil, err
			}
		}
	}

	if !writeManifest {
		return rep, cp.Save(checkpointPath)
	}
	for r, spec := range specs {
		rel := filepath.ToSlash(filepath.Join(spec.ID, "index.m3u8"))
		if err := commit("playlist:"+spec.ID, rel, b.Master.Renditions[r].MediaText()); err != nil {
			return nil, err
		}
	}
	masterBytes := b.Master.MasterText()
	masterHash := b.Master.SHA256()
	if err := commit("master", "master.m3u8", masterBytes); err != nil {
		return nil, err
	}
	cp.MasterSHA256 = masterHash
	cp.Completed = true
	if err := cp.Save(checkpointPath); err != nil {
		return nil, err
	}
	return rep, nil
}

type missingPackageError struct{ r, i int }

func (e missingPackageError) Error() string {
	return "missing CMAF package for rendition/segment"
}
func errMissingPackage(r, i int) error { return missingPackageError{r, i} }
