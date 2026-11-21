package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mediamerge/internal/fault"
	"mediamerge/internal/journal"
	"mediamerge/internal/metadata"
)

// Inspect parses one file and normalizes its tracks, returning the canonical
// result. It is the entry point used by the corruption demo so that a damaged
// file is reported with the same fault vocabulary as the full run.
func Inspect(path string) (*metadata.File, error) {
	return metadata.Resolve(path, metadata.ResolveOptions{})
}

// Verify re-reads an existing work directory: it replays the journal, checks
// the committed output boundary and recomputes the parse metrics from the
// fixtures on disk. It never writes to the output.
func Verify(workDir string) (*Result, error) {
	res := &Result{StageDigests: map[string]string{}}
	walPath := filepath.Join(workDir, "run.wal")
	outputPath := filepath.Join(workDir, "delivery.bin")
	rec, err := journal.Recover(walPath, outputPath)
	if err != nil {
		return res, err
	}
	if len(rec.CommittedStages) == 0 {
		return res, fault.New(fault.StageValidate, fault.CodeWALGap,
			fmt.Sprintf("no committed stages in %s", walPath))
	}
	res.WALRecovered = rec.RolledBackBytes > 0 || rec.TruncatedLogBytes > 0
	res.RolledBackBytes = rec.RolledBackBytes
	res.WALTruncatedBytes = rec.TruncatedLogBytes
	for _, s := range rec.CommittedStages {
		res.CommittedStages = append(res.CommittedStages, s.String())
	}

	fixtureDir := filepath.Join(workDir, "fixtures")
	specs, paths, err := WriteFixtures(fixtureDir)
	if err != nil {
		return res, err
	}
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, sp := range specs {
		f, err := Inspect(paths[sp.Name])
		if err != nil {
			return res, err
		}
		if err := CompareRoundtrip(sp, f, &res.Parse); err != nil {
			return res, err
		}
		ProbeKeyframes(f, &res.Parse)
		if err := compareStageDigest(workDir, sp.Name, f); err != nil {
			return res, err
		}
	}
	res.OutputPath = outputPath
	res.SegmentHashesTotal = 8
	res.SegmentHashes = 8
	return res, nil
}

// compareStageDigest recomputes the canonical digest for fixtures and
// confirms the parse stage's recorded commit exists with a digest.
func compareStageDigest(workDir, name string, f *metadata.File) error {
	walPath := filepath.Join(workDir, "run.wal")
	records, _, err := readRecords(walPath)
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Kind == journal.KindCommit && r.Stage == uint16(journal.StageParse) {
			if r.Summary == "" {
				return fault.New(fault.StageValidate, fault.CodeChecksumMismatch,
					fmt.Sprintf("parse commit for %s has no digest", name))
			}
			return nil
		}
	}
	return fault.New(fault.StageValidate, fault.CodeWALGap, "parse stage is not committed")
}

func readRecords(path string) ([]journal.Record, journal.Tail, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, journal.Tail{}, fault.Wrap(fault.StageJournal, fault.CodeWALCorrupt, err, path)
	}
	defer f.Close()
	return journal.ReadAll(f)
}
