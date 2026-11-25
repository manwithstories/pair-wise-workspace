package index

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"bcast/internal/fault"
	"bcast/internal/model"
	"bcast/internal/rtptrace"
	"bcast/internal/safe"
)

// Key is the deterministic cache identity. Any input, timeline or parameter
// change produces a different Hash and invalidates the old sample table.
type Key struct {
	Inputs      map[string]string `json:"inputs"`
	TimelineSHA string            `json:"timeline_sha256"`
	ParamsSHA   string            `json:"params_sha256"`
}

// NewKey builds the composite index key.
func NewKey(inputs map[string]string, tl *model.Timeline, params any) (Key, error) {
	canonical, err := tl.CanonicalJSON()
	if err != nil {
		return Key{}, err
	}
	paramsSum, _, err := hashCanonical(params)
	if err != nil {
		return Key{}, fault.E(fault.IndexInvalid, "index", "NewKey.params", err)
	}
	in := make(map[string]string, len(inputs))
	for k, v := range inputs {
		in[k] = v
	}
	return Key{Inputs: in, TimelineSHA: hashBytes(canonical), ParamsSHA: paramsSum}, nil
}

// Hash returns the stable composite key hash.
func (k Key) Hash() string {
	sum, _, _ := hashCanonical(k)
	return sum
}

// Store is the JSON index persisted under the run directory.
type Store struct {
	Version string                 `json:"version"`
	KeyHash string                 `json:"key_hash"`
	Key     Key                    `json:"key"`
	Assets  map[string]*AssetIndex `json:"assets"`
}

// LoadStore reads a store. A missing file is not an error.
func LoadStore(root *safe.Root, rel string) (*Store, bool, error) {
	b, err := root.ReadFile(rel)
	if err != nil {
		var fe *fault.Error
		if errors.As(err, &fe) && errors.Is(fe.Err, fs.ErrNotExist) {
			return nil, false, nil
		}
		// safe wraps the os error; inspect the cause chain directly too.
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var st Store
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, false, fault.E(fault.IndexInvalid, "index", "LoadStore", err).WithPath(rel)
	}
	return &st, true, nil
}

// Save writes the store atomically enough for local batch use.
func (s *Store) Save(root *safe.Root, rel string) error {
	s.Version = "bcast.index/v1"
	s.KeyHash = s.Key.Hash()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fault.E(fault.IndexInvalid, "index", "Save.marshal", err)
	}
	p, err := root.Resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fault.E(fault.WriteFailed, "index", "Save.mkdir", err).WithPath(p)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fault.E(fault.WriteFailed, "index", "Save.write", err).WithPath(tmp)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fault.E(fault.WriteFailed, "index", "Save.rename", err).WithPath(p)
	}
	return nil
}

// Matches reports whether the persisted store can serve key.
func (s *Store) Matches(key Key) bool {
	return s != nil && s.KeyHash == key.Hash()
}

// ScanResult separates usable assets from isolated rejected inputs.
type ScanResult struct {
	Store    *Store
	Accepted []*AssetIndex
	Rejected []*AssetIndex
	RTP      map[string]*rtptrace.Report
}

// ScanInputs hashes and scans every relative input path. Corrupt individual
// media assets are converted to rejected records instead of aborting siblings.
func ScanInputs(root *safe.Root, rels []string) (*ScanResult, error) {
	sort.Strings(rels)
	res := &ScanResult{
		Store: &Store{Assets: make(map[string]*AssetIndex)},
		RTP:   make(map[string]*rtptrace.Report),
	}
	seen := map[string]bool{}
	for _, rel := range rels {
		if seen[rel] {
			continue
		}
		seen[rel] = true
		abs, err := root.Resolve(rel)
		if err != nil {
			return nil, err
		}
		sum, size, herr := hashFile(abs)
		if herr != nil {
			return nil, fault.E(fault.AssetDigest, "index", "ScanInputs.hash", herr).WithPath(rel)
		}
		lower := strings.ToLower(rel)
		switch {
		case strings.HasSuffix(lower, ".mp4"):
			ai, serr := ScanMP4(abs, rel, ScanOptions{})
			if serr != nil {
				code := fault.Internal
				if fe, ok := fault.As(serr); ok {
					code = fe.Code
				}
				bad := &AssetIndex{Path: rel, SHA256: sum, Size: size, MP4: true, Rejected: true, ReasonCode: string(code), Reason: serr.Error()}
				res.Rejected = append(res.Rejected, bad)
				res.Store.Assets[rel] = bad
				continue
			}
			res.Accepted = append(res.Accepted, ai)
			res.Store.Assets[rel] = ai
		case strings.HasSuffix(lower, ".jsonl") || strings.HasSuffix(lower, ".rtp"):
			f, oerr := os.Open(abs)
			if oerr != nil {
				return nil, fault.E(fault.Internal, "index", "ScanInputs.rtp.open", oerr).WithPath(rel)
			}
			rep, perr := rtptrace.Parse(f)
			_ = f.Close()
			if perr != nil {
				code := fault.RTPInvalid
				if fe, ok := fault.As(perr); ok {
					code = fe.Code
				}
				bad := &AssetIndex{Path: rel, SHA256: sum, Size: size, Rejected: true, ReasonCode: string(code), Reason: perr.Error()}
				res.Rejected = append(res.Rejected, bad)
				res.Store.Assets[rel] = bad
				continue
			}
			res.RTP[rel] = rep
			res.Store.Assets[rel] = &AssetIndex{Path: rel, SHA256: sum, Size: size, RTPReport: rep}
		default:
			res.Store.Assets[rel] = &AssetIndex{Path: rel, SHA256: sum, Size: size}
		}
	}
	return res, nil
}

// InputHashes returns the digest map suitable for NewKey.
func (r *ScanResult) InputHashes() map[string]string {
	out := make(map[string]string, len(r.Store.Assets))
	for rel, a := range r.Store.Assets {
		out[rel] = a.SHA256
	}
	return out
}

// CountSamples returns total indexed MP4 samples.
func (s *Store) CountSamples() int {
	n := 0
	for _, a := range s.Assets {
		for _, tr := range a.Tracks {
			n += len(tr.Samples)
		}
	}
	return n
}

// CountKeyframes returns total indexed keyframe entries.
func (s *Store) CountKeyframes() int {
	n := 0
	for _, a := range s.Assets {
		for _, tr := range a.Tracks {
			n += len(tr.Keyframes)
		}
	}
	return n
}
