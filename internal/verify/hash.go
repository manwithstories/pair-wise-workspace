// Package verify re-checks delivered fragments: it recomputes each segment's
// content hash and parses the fragment back from disk so a segment that was
// written incompletely or corrupted after the fact is caught before delivery.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"

	"medialine/internal/fault"
	"medialine/internal/manifest"
	"medialine/internal/mp4"
)

// HashFile returns the sha256: prefixed content hash of a file.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fault.From(err, fault.StageVerify, fault.KindMissingIndex)
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256<<10)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := h.Write(buf[:n]); werr != nil {
				return "", fault.From(werr, fault.StageVerify, fault.KindCorruptSample)
			}
		}
		if rerr != nil {
			break
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// EntryResult is the verification outcome for one manifest row.
type EntryResult struct {
	ID      string
	File    string
	OK      bool
	Hash    string
	Skipped bool
	Err     *fault.Error
	// RoundTrip reports that the fragment parsed back with the expected
	// sample count and offsets.
	RoundTrip bool
}

// Report summarises a verification pass.
type Report struct {
	Dir          string
	Total        int
	Completed    int
	Verified     int
	Failed       int
	Skipped      int
	RoundTripped int
	Results      []EntryResult
	// Issues collects manifest-level problems such as duplicate IDs.
	Issues []string
}

// MatchRate is verified/completed fragments, or 1 when nothing completed.
func (r *Report) MatchRate() float64 {
	if r.Completed == 0 {
		return 1
	}
	return float64(r.Verified) / float64(r.Completed)
}

// Run checks every manifest entry under dir. Entries whose status is not
// "complete" are skipped rather than failed: an aborted run legitimately
// leaves pending fragments, and a manifest must never claim them.
func Run(dir string, opts mp4.Options) (*Report, error) {
	m, err := manifest.Read(dir)
	if err != nil {
		return nil, err
	}
	rep := &Report{Dir: dir}
	seen := make(map[string]bool, len(m.Entries))
	for _, e := range m.Entries {
		if seen[e.ID] {
			rep.Issues = append(rep.Issues, "duplicate segment id "+e.ID)
			continue
		}
		seen[e.ID] = true
		rep.Total++
		res := verifyEntry(dir, e, opts)
		if res.Skipped {
			rep.Skipped++
			continue
		}
		rep.Completed++
		if res.OK {
			rep.Verified++
		} else {
			rep.Failed++
		}
		if res.RoundTrip {
			rep.RoundTripped++
		}
		rep.Results = append(rep.Results, res)
	}
	sort.SliceStable(rep.Results, func(i, j int) bool { return rep.Results[i].ID < rep.Results[j].ID })
	return rep, nil
}

func verifyEntry(dir string, e manifest.Entry, opts mp4.Options) EntryResult {
	res := EntryResult{ID: e.ID, File: e.File}
	if e.Status != "complete" {
		res.Skipped = true
		return res
	}
	path, err := manifest.AbsPath(dir, e.File)
	if err != nil {
		res.Err = fault.From(err, fault.StageVerify, fault.KindPathEscape).WithSegment(e.ID)
		return res
	}
	if !pathExists(path) {
		res.Err = fault.New(fault.StageVerify, fault.KindMissingIndex,
			"segment file missing").WithSegment(e.ID)
		return res
	}
	got, err := HashFile(path)
	if err != nil {
		res.Err = fault.From(err, fault.StageVerify, fault.KindCorruptSample).WithSegment(e.ID)
		return res
	}
	res.Hash = got
	if e.ContentHash == "" {
		res.Err = fault.New(fault.StageVerify, fault.KindMissingIndex,
			"manifest has no content hash").WithSegment(e.ID)
		return res
	}
	if got != e.ContentHash {
		res.Err = fault.New(fault.StageVerify, fault.KindCorruptSample,
			"hash mismatch: got %s want %s", got, e.ContentHash).WithSegment(e.ID)
		return res
	}
	// Round-trip: the fragment must parse and describe the same sample count.
	f, perr := mp4.Parse(path, opts)
	if perr != nil {
		res.Err = fault.From(perr, fault.StageVerify, fault.KindCorruptSample).WithSegment(e.ID)
		return res
	}
	if len(f.Tracks) != 1 {
		res.Err = fault.New(fault.StageVerify, fault.KindCorruptSample,
			"fragment has %d tracks, want 1", len(f.Tracks)).WithSegment(e.ID)
		return res
	}
	if len(f.Tracks[0].Samples) != len(e.SampleNumbers) {
		res.Err = fault.New(fault.StageVerify, fault.KindCorruptSample,
			"fragment has %d samples, manifest lists %d", len(f.Tracks[0].Samples), len(e.SampleNumbers)).
			WithSegment(e.ID)
		return res
	}
	res.RoundTrip = true
	res.OK = true
	return res
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// HashSegments hashes every completed fragment in a manifest and reports which
// files are missing. It is a cheaper check than Run when round-tripping is not
// needed.
func HashSegments(dir string) (map[string]string, []string, error) {
	m, err := manifest.Read(dir)
	if err != nil {
		return nil, nil, err
	}
	hashes := make(map[string]string)
	var missing []string
	for _, e := range m.Entries {
		if e.Status != "complete" {
			continue
		}
		path, err := manifest.AbsPath(dir, e.File)
		if err != nil || !pathExists(path) {
			missing = append(missing, e.ID)
			continue
		}
		h, err := HashFile(path)
		if err != nil {
			missing = append(missing, e.ID)
			continue
		}
		hashes[e.ID] = h
	}
	sort.Strings(missing)
	return hashes, missing, nil
}

// RelUnderRoot reports whether rel stays under root.
func RelUnderRoot(root, rel string) bool {
	p := filepath.Clean(filepath.Join(root, rel))
	return p == root || (len(p) > len(root) && p[:len(root)] == root && p[len(root)] == filepath.Separator)
}
