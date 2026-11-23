package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"mediaroute/internal/fault"
	"mediaroute/internal/probe"
)

// onDisk is the full file envelope written to disk.
type onDisk struct {
	Header Header `json:"header"`
	Body   Body   `json:"body"`
}

// Store reads and writes index files with atomic commit semantics: data
// is always written to a sibling temp file and renamed into place, so a
// crash can never leave a half-written index at the target path.
type Store struct {
	dir    string
	params Params
}

// NewStore creates a store rooted at dir (created lazily on write).
func NewStore(dir string, params Params) *Store {
	return &Store{dir: dir, params: params}
}

// path returns the canonical index location for an input file name.
func (s *Store) path(inputPath string) string {
	base := filepath.Base(inputPath)
	return filepath.Join(s.dir, base+".midx")
}

// Save serialises and atomically commits an index for the report.
func (s *Store) Save(inputPath string, r *probe.ContainerReport) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fault.New(fault.CatIO, "create index dir: %v", err)
	}
	body := &Body{Report: r}
	sum, err := checksum(body)
	if err != nil {
		return fault.New(fault.CatIO, "index checksum: %v", err)
	}
	hdr := Header{
		Magic:        Magic,
		IndexVersion: IndexVersion,
		Scanner:      probe.ScannerVersion,
		InputSHA256:  r.InputSHA256,
		Params:       s.params,
		BodySHA256:   sum,
	}
	doc := onDisk{Header: hdr, Body: *body}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fault.New(fault.CatIO, "marshal index: %v", err)
	}
	target := s.path(inputPath)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fault.New(fault.CatIO, "write temp index: %v", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fault.New(fault.CatIO, "commit index: %v", err)
	}
	return nil
}

// Load reads and fully validates an index. A truncated file, bad magic,
// version mismatch, checksum failure or fingerprint mismatch returns
// CatIndexInvalid so the caller can safely re-scan; the index is never
// partially consumed.
func (s *Store) Load(inputPath, inputSHA string) (*probe.ContainerReport, Fingerprint, error) {
	target := s.path(inputPath)
	raw, err := os.ReadFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			// Preserve the not-exist sentinel via Cause so callers can
			// distinguish a first-run missing index from a rejected one.
			return nil, Fingerprint{}, fault.New(fault.CatIndexInvalid,
				"index unreadable: %v", err).Wrap(err)
		}
		return nil, Fingerprint{}, fault.New(fault.CatIndexInvalid,
			"index unreadable: %v", err)
	}
	var doc onDisk
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, Fingerprint{}, fault.New(fault.CatIndexInvalid,
			"index truncated or corrupt: %v", err)
	}
	h := doc.Header
	if h.Magic != Magic || h.IndexVersion != IndexVersion {
		return nil, Fingerprint{}, fault.New(fault.CatIndexInvalid,
			"index magic/version mismatch (magic=%q ver=%d)",
			h.Magic, h.IndexVersion)
	}
	want, err := checksum(&doc.Body)
	if err != nil || h.BodySHA256 != want {
		return nil, Fingerprint{}, fault.New(fault.CatIndexInvalid,
			"index body checksum mismatch")
	}
	got := Fingerprint{
		InputSHA256: h.InputSHA256, Scanner: h.Scanner, Params: h.Params,
	}
	wantFP := Fingerprint{
		InputSHA256: inputSHA, Scanner: probe.ScannerVersion, Params: s.params,
	}
	if !got.Matches(wantFP) {
		return nil, got, fault.New(fault.CatIndexInvalid,
			"fingerprint mismatch: got {%s} want {%s}", got, wantFP)
	}
	if doc.Body.Report == nil || doc.Body.Report.InputSHA256 != inputSHA {
		return nil, got, fault.New(fault.CatIndexInvalid,
			"index report does not describe this input")
	}
	return doc.Body.Report, got, nil
}

// Invalidate removes a bad index so a fresh scan replaces it. A missing
// file is not an error.
func (s *Store) Invalidate(inputPath string) error {
	target := s.path(inputPath)
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fault.New(fault.CatIO, "invalidate index: %v", err)
	}
	return nil
}

// String renders a fingerprint for diagnostics.
func (s *Store) String() string {
	return fmt.Sprintf("Store(dir=%s)", s.dir)
}
